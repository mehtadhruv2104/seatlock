package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dhruvmehta/seatlock/internal/config"
	"github.com/dhruvmehta/seatlock/internal/db"
	"github.com/dhruvmehta/seatlock/internal/handlers"
	"github.com/dhruvmehta/seatlock/internal/logging"
	"github.com/dhruvmehta/seatlock/internal/metrics"
	"github.com/dhruvmehta/seatlock/internal/router"
	"github.com/dhruvmehta/seatlock/internal/service"
	"github.com/gin-gonic/gin"
)

func main() {
	logging.Init()
	// Gin's debug mode prints plain-text banners into otherwise JSON logs.
	if os.Getenv("GIN_MODE") == "" {
		gin.SetMode(gin.ReleaseMode)
	}

	cfg, err := config.Load()
	if err != nil {
		fatal("invalid configuration", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		fatal("database unreachable", err)
	}
	defer pool.Close()

	if err := db.Migrate(ctx, pool); err != nil {
		fatal("migrations failed", err)
	}

	store := db.NewStore(pool)
	m := metrics.New(store)
	h := handlers.New(store, service.NewShowService(store), service.NewReservationService(store), m)

	srv := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: router.New(h, m, cfg.AdminKey),
	}

	go func() {
		slog.Info("listening", "port", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fatal("server error", err)
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down")

	// Let in-flight requests (e.g. a reserve transaction) finish before exiting.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("forced shutdown", "error", err.Error())
	}
}

func fatal(msg string, err error) {
	slog.Error(msg, "error", err.Error())
	os.Exit(1)
}
