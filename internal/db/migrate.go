package db

import (
	"context"
	"embed"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Migrate applies pending migrations. Embedded in the binary so a fresh
// deploy creates its own schema with no separate migration step.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	sqlDB := stdlib.OpenDBFromPool(pool)
	defer sqlDB.Close()

	goose.SetLogger(gooseLogger{})
	goose.SetBaseFS(migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set dialect: %w", err)
	}
	if err := goose.UpContext(ctx, sqlDB, "migrations"); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

// gooseLogger sends goose's output through the JSON logger.
type gooseLogger struct{}

func (gooseLogger) Printf(format string, v ...any) {
	slog.Info(strings.TrimSpace(fmt.Sprintf(format, v...)), "component", "migrations")
}

func (gooseLogger) Fatalf(format string, v ...any) {
	slog.Error(strings.TrimSpace(fmt.Sprintf(format, v...)), "component", "migrations")
	os.Exit(1)
}
