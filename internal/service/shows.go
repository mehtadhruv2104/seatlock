package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/dhruvmehta/seatlock/internal/db"
	"github.com/dhruvmehta/seatlock/internal/model"
)

const (
	DefaultPerUserLimit = 4
	MaxSeatsPerShow     = 100_000
	MaxSeatLabelLen     = 32
	MaxShowNameLen      = 200
	maxExamples         = 10 // cap on offending values echoed back in an error
)

type ShowService struct {
	store *db.Store
}

func NewShowService(store *db.Store) *ShowService {
	return &ShowService{store: store}
}

type CreateShowInput struct {
	Name         string
	Seats        []string
	PricePaise   int64
	PerUserLimit *int
}

func (s *ShowService) CreateShow(ctx context.Context, in CreateShowInput) (model.ShowDetail, error) {
	in.Name = strings.TrimSpace(in.Name)
	limit := DefaultPerUserLimit
	if in.PerUserLimit != nil {
		limit = *in.PerUserLimit
	}
	if err := validateCreateShow(in, limit); err != nil {
		return model.ShowDetail{}, err
	}

	show, err := s.store.CreateShow(ctx, in.Name, in.Seats, in.PricePaise, limit)
	if err != nil {
		return model.ShowDetail{}, err
	}

	// Every seat of a new show is available; no need to read them back.
	seats := make([]model.Seat, len(in.Seats))
	for i, label := range in.Seats {
		seats[i] = model.Seat{Label: label, Status: model.SeatAvailable}
	}
	return model.ShowDetail{
		Show:   show,
		Counts: model.SeatCounts{Available: len(seats), Total: len(seats)},
		Seats:  seats,
	}, nil
}

func validateCreateShow(in CreateShowInput, limit int) error {
	if in.Name == "" {
		return &ValidationError{Field: "name", Message: "name must not be empty."}
	}
	if len(in.Name) > MaxShowNameLen {
		return &ValidationError{Field: "name", Message: fmt.Sprintf("name must be at most %d characters.", MaxShowNameLen)}
	}

	if len(in.Seats) == 0 {
		return &ValidationError{Field: "seats", Message: "seats must contain at least one seat label."}
	}
	if len(in.Seats) > MaxSeatsPerShow {
		return &ValidationError{
			Field:   "seats",
			Message: fmt.Sprintf("A show can have at most %d seats; got %d.", MaxSeatsPerShow, len(in.Seats)),
		}
	}

	var invalid, duplicates []string
	seen := make(map[string]bool, len(in.Seats))
	reported := make(map[string]bool)
	for _, label := range in.Seats {
		if label == "" || label != strings.TrimSpace(label) || len(label) > MaxSeatLabelLen {
			if len(invalid) < maxExamples {
				invalid = append(invalid, label)
			}
			continue
		}
		if seen[label] && !reported[label] {
			reported[label] = true
			if len(duplicates) < maxExamples {
				duplicates = append(duplicates, label)
			}
		}
		seen[label] = true
	}
	if len(invalid) > 0 {
		return &ValidationError{
			Field: "seats",
			Message: fmt.Sprintf("Seat labels must be 1-%d characters with no leading or trailing spaces.",
				MaxSeatLabelLen),
			Details: map[string]any{"invalid_seats": invalid},
		}
	}
	if len(duplicates) > 0 {
		return &ValidationError{
			Field:   "seats",
			Message: "Each seat label must be unique within a show.",
			Details: map[string]any{"duplicate_seats": duplicates},
		}
	}

	if in.PricePaise < 0 {
		return &ValidationError{Field: "price_paise", Message: "price_paise must be zero or positive."}
	}
	if limit < 1 {
		return &ValidationError{Field: "per_user_limit", Message: "per_user_limit must be at least 1."}
	}
	return nil
}

func (s *ShowService) GetShow(ctx context.Context, id string) (model.ShowDetail, error) {
	show, err := s.store.GetShow(ctx, id)
	if errors.Is(err, db.ErrNotFound) {
		return model.ShowDetail{}, ErrShowNotFound
	}
	if err != nil {
		return model.ShowDetail{}, err
	}

	seats, err := s.store.ListSeats(ctx, id)
	if err != nil {
		return model.ShowDetail{}, err
	}

	// Counted from the same rows we return, so available + held + confirmed
	// == total by construction (the reconciliation invariant).
	counts := model.SeatCounts{Total: len(seats)}
	for _, seat := range seats {
		switch seat.Status {
		case model.SeatAvailable:
			counts.Available++
		case model.SeatHeld:
			counts.Held++
		case model.SeatConfirmed:
			counts.Confirmed++
		}
	}
	return model.ShowDetail{Show: show, Counts: counts, Seats: seats}, nil
}
