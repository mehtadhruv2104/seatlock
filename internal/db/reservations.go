package db

import (
	"context"
	"errors"

	"github.com/dhruvmehta/seatlock/internal/model"
	"github.com/jackc/pgx/v5"
)

// ClaimReservation inserts the reservation row that claims this idempotency
// key. claimed is false if a committed reservation with the same
// (user, key) already exists. If another transaction holding the same key is
// still in flight, this blocks on the unique index until it finishes.
func (t *Tx) ClaimReservation(ctx context.Context, showID, userID, key string, seats []string) (id string, claimed bool, err error) {
	err = t.tx.QueryRow(ctx, `
		INSERT INTO reservations (show_id, user_id, idempotency_key, seats, amount_paise, status)
		VALUES ($1, $2, $3, $4, 0, 'confirmed')
		ON CONFLICT (user_id, idempotency_key) DO NOTHING
		RETURNING id`,
		showID, userID, key, seats,
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return id, true, nil
}

func (t *Tx) GetReservationByKey(ctx context.Context, userID, key string) (model.Reservation, error) {
	var r model.Reservation
	err := t.tx.QueryRow(ctx, `
		SELECT id, show_id, user_id, seats, amount_paise, status
		FROM reservations WHERE user_id = $1 AND idempotency_key = $2`,
		userID, key,
	).Scan(&r.ID, &r.ShowID, &r.UserID, &r.Seats, &r.AmountPaise, &r.Status)
	return r, err
}

// LockUser takes the per-(show, user) lock row, creating it on first use, so
// one user's reservations for one show run one at a time.
func (t *Tx) LockUser(ctx context.Context, showID, userID string) error {
	if _, err := t.tx.Exec(ctx, `
		INSERT INTO user_show_locks (show_id, user_id) VALUES ($1, $2)
		ON CONFLICT DO NOTHING`, showID, userID); err != nil {
		return err
	}
	_, err := t.tx.Exec(ctx, `
		SELECT 1 FROM user_show_locks WHERE show_id = $1 AND user_id = $2
		FOR UPDATE`, showID, userID)
	return err
}

func (t *Tx) CountConfirmedSeats(ctx context.Context, showID, userID string) (int, error) {
	var n int
	err := t.tx.QueryRow(ctx, `
		SELECT count(*) FROM seats
		WHERE show_id = $1 AND user_id = $2 AND status = 'confirmed'`,
		showID, userID,
	).Scan(&n)
	return n, err
}

type LockedSeat struct {
	ID     string
	Label  string
	Status model.SeatStatus
}

// LockSeats row-locks the requested seats in seat_label order. Every
// transaction locks in the same order, which is what rules out deadlocks
// between overlapping multi-seat requests (DESIGN.md §4). Labels that don't
// exist in the show are simply absent from the result.
func (t *Tx) LockSeats(ctx context.Context, showID string, labels []string) ([]LockedSeat, error) {
	rows, err := t.tx.Query(ctx, `
		SELECT id, seat_label, status FROM seats
		WHERE show_id = $1 AND seat_label = ANY($2)
		ORDER BY seat_label
		FOR UPDATE`, showID, labels)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (LockedSeat, error) {
		var s LockedSeat
		err := row.Scan(&s.ID, &s.Label, &s.Status)
		return s, err
	})
}

// ConfirmSeats is state-guarded (status = 'available') as a safeguard: callers
// must check the returned count equals the number of seats they expect.
func (t *Tx) ConfirmSeats(ctx context.Context, seatIDs []string, userID, reservationID string) (int64, error) {
	tag, err := t.tx.Exec(ctx, `
		UPDATE seats SET status = 'confirmed', user_id = $2, reservation_id = $3
		WHERE id = ANY($1::uuid[]) AND status = 'available'`,
		seatIDs, userID, reservationID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (t *Tx) SetReservationAmount(ctx context.Context, reservationID string, amountPaise int64) error {
	_, err := t.tx.Exec(ctx, `UPDATE reservations SET amount_paise = $2 WHERE id = $1`,
		reservationID, amountPaise)
	return err
}
