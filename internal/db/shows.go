package db

import (
	"context"
	"fmt"

	"github.com/dhruvmehta/seatlock/internal/model"
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
