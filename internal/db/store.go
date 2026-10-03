package db

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("not found")

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Ping checks the database answers within timeout, for the readiness probe.
func (s *Store) Ping(ctx context.Context, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return s.pool.Ping(ctx)
}

// PoolStat exposes connection-pool statistics for metrics.
func (s *Store) PoolStat() *pgxpool.Stat {
	return s.pool.Stat()
}
