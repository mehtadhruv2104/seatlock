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

// Decline reasons. Also used as the metric label, so treat them as stable API.
const (
	ReasonSeatTaken           = "seat_taken"
	ReasonPerUserLimit        = "per_user_limit"
	ReasonIdempotencyConflict = "idempotent_replay_conflict"
)

// DeclineError is a clean domain "no" (HTTP 409), never a server error.
type DeclineError struct {
	Reason  string
	Message string
	Details map[string]any
}

func (e *DeclineError) Error() string { return e.Message }
