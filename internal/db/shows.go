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

// GetShowWithSeats loads the show and whichever of the requested seat labels
// exist in it, in one round trip. Unlocked read: it's used to reject unknown
// labels before any transaction starts, which is safe because a show's seats
// never change after creation. Labels that don't exist are absent from the result.
func (s *Store) GetShowWithSeats(ctx context.Context, id string, labels []string) (model.Show, []string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT sh.id, sh.name, sh.price_paise, sh.per_user_limit, sh.created_at, se.seat_label
		FROM shows sh
		LEFT JOIN seats se ON se.show_id = sh.id AND se.seat_label = ANY($2)
		WHERE sh.id = $1`, id, labels)
	if err != nil {
		return model.Show{}, nil, err
	}
	defer rows.Close()

	var show model.Show
	var found []string
	sawShow := false
	for rows.Next() {
		var label *string
		if err := rows.Scan(&show.ID, &show.Name, &show.PricePaise, &show.PerUserLimit, &show.CreatedAt, &label); err != nil {
			return model.Show{}, nil, err
		}
		sawShow = true
		if label != nil {
			found = append(found, *label)
		}
	}
	if err := rows.Err(); err != nil {
		return model.Show{}, nil, err
	}
	if !sawShow {
		return model.Show{}, nil, ErrNotFound
	}
	return show, found, nil
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
