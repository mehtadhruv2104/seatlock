package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/dhruvmehta/seatlock/internal/model"
	"github.com/jackc/pgx/v5"
)

// CreateShow inserts the show and all its seats in one transaction, so a show
// never exists with only some of its seats.
func (s *Store) CreateShow(ctx context.Context, name string, labels []string, pricePaise int64, perUserLimit int) (model.Show, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return model.Show{}, err
	}
	defer tx.Rollback(ctx)

	var show model.Show
	err = tx.QueryRow(ctx, `
		INSERT INTO shows (name, price_paise, per_user_limit)
		VALUES ($1, $2, $3)
		RETURNING id, name, price_paise, per_user_limit, created_at`,
		name, pricePaise, perUserLimit,
	).Scan(&show.ID, &show.Name, &show.PricePaise, &show.PerUserLimit, &show.CreatedAt)
	if err != nil {
		return model.Show{}, fmt.Errorf("insert show: %w", err)
	}

	// One statement for all seats; WITH ORDINALITY records each label's
	// position in the admin's list.
	_, err = tx.Exec(ctx, `
		INSERT INTO seats (show_id, seat_label, position)
		SELECT $1, label, ord
		FROM unnest($2::text[]) WITH ORDINALITY AS t(label, ord)`,
		show.ID, labels,
	)
	if err != nil {
		return model.Show{}, fmt.Errorf("insert seats: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return model.Show{}, err
	}
	return show, nil
}

func (s *Store) GetShow(ctx context.Context, id string) (model.Show, error) {
	var show model.Show
	err := s.pool.QueryRow(ctx, `
		SELECT id, name, price_paise, per_user_limit, created_at
		FROM shows WHERE id = $1`, id,
	).Scan(&show.ID, &show.Name, &show.PricePaise, &show.PerUserLimit, &show.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Show{}, ErrNotFound
	}
	return show, err
}

// ListSeats returns every seat of a show in the admin's original order, read
// in one statement so all seats come from the same snapshot.
func (s *Store) ListSeats(ctx context.Context, showID string) ([]model.Seat, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT seat_label, status FROM seats
		WHERE show_id = $1
		ORDER BY position`, showID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.Seat, error) {
		var seat model.Seat
		err := row.Scan(&seat.Label, &seat.Status)
		return seat, err
	})
}

// PrecheckSeat is a requested seat as seen by the unlocked step-0 read.
type PrecheckSeat struct {
	Label  string
	Status model.SeatStatus
}

// ReservePrecheck is everything reserve reads before its transaction.
type ReservePrecheck struct {
	Show  model.Show
	Seats []PrecheckSeat // requested labels that exist in the show
	// KeyUsed is true if this user already has a reservation with this
	// idempotency key: the request is a retry or a reused key.
	KeyUsed bool
}

// GetReservePrecheck loads the show, the requested seats with their current
// status, and whether the idempotency key is already used, in one statement:
// one round trip, and one snapshot, so the seat statuses and KeyUsed are
// consistent with each other. Unlocked: callers may use it only to decline
// (unknown or already-sold seats), never to grant. Seat existence is safe to
// check here because a show's seats never change after creation.
func (s *Store) GetReservePrecheck(ctx context.Context, showID string, labels []string, userID, key string) (ReservePrecheck, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT sh.id, sh.name, sh.price_paise, sh.per_user_limit, sh.created_at,
		       se.seat_label, se.status,
		       EXISTS (SELECT 1 FROM reservations r WHERE r.user_id = $3 AND r.idempotency_key = $4)
		FROM shows sh
		LEFT JOIN seats se ON se.show_id = sh.id AND se.seat_label = ANY($2)
		WHERE sh.id = $1`, showID, labels, userID, key)
	if err != nil {
		return ReservePrecheck{}, err
	}
	defer rows.Close()

	var pc ReservePrecheck
	sawShow := false
	for rows.Next() {
		var label, status *string
		if err := rows.Scan(&pc.Show.ID, &pc.Show.Name, &pc.Show.PricePaise, &pc.Show.PerUserLimit,
			&pc.Show.CreatedAt, &label, &status, &pc.KeyUsed); err != nil {
			return ReservePrecheck{}, err
		}
		sawShow = true
		if label != nil {
			pc.Seats = append(pc.Seats, PrecheckSeat{Label: *label, Status: model.SeatStatus(*status)})
		}
	}
	if err := rows.Err(); err != nil {
		return ReservePrecheck{}, err
	}
	if !sawShow {
		return ReservePrecheck{}, ErrNotFound
	}
	return pc, nil
}

type SeatTotals struct {
	Available, Held, Confirmed, Total int
}

// SeatTotals counts seats by status across all shows in one statement, so the
// four numbers come from one snapshot and always satisfy
// available + held + confirmed == total.
func (s *Store) SeatTotals(ctx context.Context) (SeatTotals, error) {
	var t SeatTotals
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE status = 'available'),
		       count(*) FILTER (WHERE status = 'held'),
		       count(*) FILTER (WHERE status = 'confirmed'),
		       count(*)
		FROM seats`).Scan(&t.Available, &t.Held, &t.Confirmed, &t.Total)
	return t, err
}
