package handlers

import (
	"errors"
	"net/http"

	"github.com/dhruvmehta/seatlock/internal/metrics"
	"github.com/dhruvmehta/seatlock/internal/middleware"
	"github.com/dhruvmehta/seatlock/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// No user_id field: identity comes only from the token, so a spoofed
// user_id in the body is never even read.
type reserveRequest struct {
	Seats          []string `json:"seats"`
	IdempotencyKey *string  `json:"idempotency_key"`
}

const keyHint = "Generate a UUID once per booking and reuse it only when retrying that same booking."

func (h *Handlers) Reserve(c *gin.Context) {
	showID := c.Param("id")
	if _, err := uuid.Parse(showID); err != nil {
		writeError(c, http.StatusBadRequest, "validation_error",
			"Show id must be a UUID, as returned by POST /shows.", map[string]any{"field": "id"})
		return
	}

	var req reserveRequest
	if !decodeJSON(c, &req) {
		return
	}
	if req.Seats == nil {
		writeError(c, http.StatusBadRequest, "validation_error", "seats is required.", map[string]any{"field": "seats"})
		return
	}
	key, ok := idempotencyKey(c, req.IdempotencyKey)
	if !ok {
		return
	}

	c.Set(middleware.ShowIDKey, showID)
	result, err := h.reservations.Reserve(c.Request.Context(), service.ReserveInput{
		ShowID:         showID,
		UserID:         middleware.UserID(c),
		IdempotencyKey: key,
		Seats:          req.Seats,
	})
	// Business metrics are counted here, once the outcome is final, so they
	// match the responses clients actually received.
	var ve *service.ValidationError
	var de *service.DeclineError
	switch {
	case errors.As(err, &ve):
		writeValidationError(c, ve)
	case errors.As(err, &de):
		h.metrics.Declined(de.Reason)
		if de.SafeguardTripped {
			h.metrics.SafeguardTripped()
		}
		writeDecline(c, de)
	case errors.Is(err, service.ErrShowNotFound):
		writeError(c, http.StatusNotFound, "show_not_found", "No show exists with this id.",
			map[string]any{"show_id": showID})
	case err != nil:
		writeInternalError(c, err)
	case result.Replayed:
		h.metrics.Declined(metrics.ReasonIdempotentReplay)
		c.Set(middleware.ReservationIDKey, result.Reservation.ID)
		c.JSON(http.StatusOK, result.Reservation)
	default:
		h.metrics.ReservationConfirmed(len(result.Reservation.Seats))
		c.Set(middleware.ReservationIDKey, result.Reservation.ID)
		c.JSON(http.StatusCreated, result.Reservation)
	}
}

// idempotencyKey accepts the key from the body or the Idempotency-Key header
// and normalizes it to the canonical lowercase UUID form, so "ABC..." and
// "abc..." are the same key. Writes a 400 and returns false on failure.
func idempotencyKey(c *gin.Context, fromBody *string) (string, bool) {
	header := c.GetHeader("Idempotency-Key")
	var raw string
	switch {
	case fromBody != nil && header != "" && *fromBody != header:
		writeError(c, http.StatusBadRequest, "validation_error",
			"idempotency_key in the body and the Idempotency-Key header differ. Send one, or make them match.",
			map[string]any{"field": "idempotency_key"})
		return "", false
	case fromBody != nil:
		raw = *fromBody
	case header != "":
		raw = header
	default:
		writeError(c, http.StatusBadRequest, "validation_error",
			"idempotency_key is required (in the body or the Idempotency-Key header). "+keyHint,
			map[string]any{"field": "idempotency_key"})
		return "", false
	}

	parsed, err := uuid.Parse(raw)
	if err != nil {
		writeError(c, http.StatusBadRequest, "validation_error",
			"idempotency_key must be a UUID. "+keyHint, map[string]any{"field": "idempotency_key"})
		return "", false
	}
	return parsed.String(), true
}

func (h *Handlers) Cancel(c *gin.Context) {
	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		writeError(c, http.StatusBadRequest, "validation_error",
			"Reservation id must be a UUID, as returned by POST /shows/{id}/reserve.",
			map[string]any{"field": "id"})
		return
	}

	c.Set(middleware.ReservationIDKey, id)
	result, err := h.reservations.Cancel(c.Request.Context(), middleware.UserID(c), id)
	switch {
	case errors.Is(err, service.ErrReservationNotFound):
		writeError(c, http.StatusNotFound, "reservation_not_found",
			"No reservation with this id exists for your account.", map[string]any{"reservation_id": id})
	case err != nil:
		writeInternalError(c, err)
	default:
		if !result.AlreadyCancelled {
			h.metrics.ReservationCancelled(len(result.Reservation.Seats))
		}
		c.Set(middleware.ShowIDKey, result.Reservation.ShowID)
		c.JSON(http.StatusOK, result.Reservation)
	}
}
