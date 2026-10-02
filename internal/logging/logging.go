package logging

import (
	"context"
	"log/slog"
	"os"
)

type ctxKey struct{}

// Init installs a JSON logger as the process default.
func Init() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
}

// WithLogger attaches a request-scoped logger (carrying the request id) to ctx.
func WithLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, l)
}

// FromContext returns the request-scoped logger, or the default logger
// outside a request.
func FromContext(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}
