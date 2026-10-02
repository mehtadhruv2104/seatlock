package handlers

import (
	"errors"
	"net/http"

	"github.com/dhruvmehta/seatlock/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Pointers distinguish "field missing" from "field set to zero value".
type createShowRequest struct {
	Name         *string  `json:"name"`
	Seats        []string `json:"seats"`
	PricePaise   *int64   `json:"price_paise"`
	PerUserLimit *int     `json:"per_user_limit"`
}

func (h *Handlers) CreateShow(c *gin.Context) {
	var req createShowRequest
	if !decodeJSON(c, &req) {
		return
	}

	switch {
	case req.Name == nil:
		writeError(c, http.StatusBadRequest, "validation_error", "name is required.", map[string]any{"field": "name"})
		return
	case req.Seats == nil:
		writeError(c, http.StatusBadRequest, "validation_error", "seats is required.", map[string]any{"field": "seats"})
		return
	case req.PricePaise == nil:
		writeError(c, http.StatusBadRequest, "validation_error",
			"price_paise is required, as a whole number of paise (e.g. 25000 for ₹250).",
			map[string]any{"field": "price_paise"})
		return
	}

	detail, err := h.shows.CreateShow(c.Request.Context(), service.CreateShowInput{
		Name:         *req.Name,
		Seats:        req.Seats,
		PricePaise:   *req.PricePaise,
		PerUserLimit: req.PerUserLimit,
	})
	var ve *service.ValidationError
	switch {
	case errors.As(err, &ve):
		writeValidationError(c, ve)
	case err != nil:
		writeInternalError(c, err)
	default:
		c.JSON(http.StatusCreated, detail)
	}
}

func (h *Handlers) GetShow(c *gin.Context) {
	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		writeError(c, http.StatusBadRequest, "validation_error",
			"Show id must be a UUID, as returned by POST /shows.", map[string]any{"field": "id"})
		return
	}

	detail, err := h.shows.GetShow(c.Request.Context(), id)
	switch {
	case errors.Is(err, service.ErrShowNotFound):
		writeError(c, http.StatusNotFound, "show_not_found", "No show exists with this id.",
			map[string]any{"show_id": id})
	case err != nil:
		writeInternalError(c, err)
	default:
		c.JSON(http.StatusOK, detail)
	}
}
