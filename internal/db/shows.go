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
