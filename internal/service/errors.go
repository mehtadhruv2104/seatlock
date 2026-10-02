package service

import "errors"

var ErrShowNotFound = errors.New("show not found")

// ValidationError is a client mistake (HTTP 400). Details carries the specific
// offending values so the client can fix the request without guessing.
type ValidationError struct {
	Field   string
	Message string
	Details map[string]any
}

func (e *ValidationError) Error() string { return e.Message }
