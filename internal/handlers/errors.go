package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"reflect"
	"runtime/debug"

	"github.com/dhruvmehta/seatlock/internal/logging"
	"github.com/dhruvmehta/seatlock/internal/middleware"
	"github.com/dhruvmehta/seatlock/internal/service"
	"github.com/gin-gonic/gin"
)

// writeError renders the standard error body from DESIGN.md §9:
// a machine-readable reason, a human message, and any context fields.
func writeError(c *gin.Context, status int, reason, message string, extra map[string]any) {
	body := gin.H{"reason": reason, "message": message}
	for k, v := range extra {
		body[k] = v
	}
	c.Set(middleware.ReasonKey, reason)
	c.JSON(status, body)
}

func writeValidationError(c *gin.Context, ve *service.ValidationError) {
	extra := map[string]any{"field": ve.Field}
	for k, v := range ve.Details {
		extra[k] = v
	}
	writeError(c, http.StatusBadRequest, "validation_error", ve.Message, extra)
}

func writeDecline(c *gin.Context, de *service.DeclineError) {
	writeError(c, http.StatusConflict, de.Reason, de.Message, de.Details)
}

func writeInternalError(c *gin.Context, err error) {
	logging.FromContext(c.Request.Context()).Error("internal error", "route", c.FullPath(), "error", err.Error())
	writeError(c, http.StatusInternalServerError, "internal_error",
		"Something went wrong on our side. Please retry.", nil)
}

// decodeJSON reads the request body into dst and, on failure, writes a 4xx
// that names the problem. Returns false if the request was already answered.
// A body that never fully arrived (read timeout, connection cut mid-body) is
// reported as such, not as malformed JSON: blaming the client's JSON would
// hide a network or timeout problem behind a "client bug" label.
func decodeJSON(c *gin.Context, dst any) bool {
	err := json.NewDecoder(c.Request.Body).Decode(dst)
	if err == nil {
		return true
	}

	var typeErr *json.UnmarshalTypeError
	var tooLarge *http.MaxBytesError
	var netErr net.Error
	switch {
	case errors.As(err, &tooLarge):
		writeError(c, http.StatusRequestEntityTooLarge, "payload_too_large",
			fmt.Sprintf("Request body exceeds the %d MiB limit.", tooLarge.Limit>>20), nil)
	case errors.As(err, &netErr) && netErr.Timeout():
		writeError(c, http.StatusRequestTimeout, "request_timeout",
			"Timed out waiting for the request body. Retry the request (with the same idempotency key).", nil)
	case errors.Is(err, io.ErrUnexpectedEOF):
		writeError(c, http.StatusBadRequest, "incomplete_body",
			"The request body ended before the JSON was complete. Send the full body.", nil)
	case errors.Is(err, io.EOF):
		writeError(c, http.StatusBadRequest, "validation_error", "Request body is required.", nil)
	case errors.As(err, &typeErr):
		writeError(c, http.StatusBadRequest, "validation_error",
			typeErrorMessage(typeErr), map[string]any{"field": typeErr.Field})
	default:
		writeError(c, http.StatusBadRequest, "validation_error", "Request body is not valid JSON.", nil)
	}
	return false
}

// typeErrorMessage describes the expected type in client terms, not Go terms.
func typeErrorMessage(e *json.UnmarshalTypeError) string {
	if e.Field == "price_paise" {
		return "price_paise must be a whole number of paise (e.g. 25000 for ₹250). Decimals and strings are not accepted."
	}
	expected := "a different type"
	switch e.Type.Kind() {
	case reflect.Int, reflect.Int64:
		expected = "a whole number"
	case reflect.String:
		expected = "a string"
	case reflect.Slice:
		expected = "a list"
	}
	return fmt.Sprintf("%s must be %s.", e.Field, expected)
}

func NotFound(c *gin.Context) {
	writeError(c, http.StatusNotFound, "route_not_found",
		fmt.Sprintf("No endpoint at %s %s.", c.Request.Method, c.Request.URL.Path), nil)
}

func MethodNotAllowed(c *gin.Context) {
	writeError(c, http.StatusMethodNotAllowed, "method_not_allowed",
		fmt.Sprintf("%s is not supported on %s.", c.Request.Method, c.Request.URL.Path), nil)
}

// Recover turns a panic into a JSON 500 and logs it with the stack and the
// request id, instead of an empty response.
func Recover(c *gin.Context, recovered any) {
	logging.FromContext(c.Request.Context()).Error("panic",
		"panic", fmt.Sprint(recovered), "stack", string(debug.Stack()))
	writeError(c, http.StatusInternalServerError, "internal_error",
		"Something went wrong on our side. Please retry.", nil)
	c.Abort()
}
