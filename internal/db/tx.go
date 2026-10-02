package db

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// Tx exposes transaction-scoped queries without leaking pgx to callers.
type Tx struct {
	tx pgx.Tx
}

// WithTx runs fn in one transaction at Postgres's default READ COMMITTED
// level. If fn returns an error everything is rolled back; otherwise it commits.
func (s *Store) WithTx(ctx context.Context, fn func(*Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) // no-op after a successful commit

	if err := fn(&Tx{tx: tx}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
