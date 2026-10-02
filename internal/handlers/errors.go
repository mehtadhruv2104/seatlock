package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"reflect"

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
	c.JSON(status, body)
}

func writeValidationError(c *gin.Context, ve *service.ValidationError) {
	extra := map[string]any{"field": ve.Field}
	for k, v := range ve.Details {
		extra[k] = v
	}
	writeError(c, http.StatusBadRequest, "validation_error", ve.Message, extra)
}

func writeInternalError(c *gin.Context, err error) {
	log.Printf("internal error on %s %s: %v", c.Request.Method, c.FullPath(), err)
	writeError(c, http.StatusInternalServerError, "internal_error",
		"Something went wrong on our side. Please retry.", nil)
}

// decodeJSON reads the request body into dst and, on failure, writes a 400
// that names the problem. Returns false if the request was already answered.
func decodeJSON(c *gin.Context, dst any) bool {
	err := json.NewDecoder(c.Request.Body).Decode(dst)
	if err == nil {
		return true
	}

	var typeErr *json.UnmarshalTypeError
	switch {
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
