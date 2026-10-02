package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/dhruvmehta/seatlock/internal/config"
	"github.com/dhruvmehta/seatlock/internal/db"
	"github.com/dhruvmehta/seatlock/internal/handlers"
	"github.com/dhruvmehta/seatlock/internal/router"
	"github.com/dhruvmehta/seatlock/internal/service"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("database: %v", err)
	}
	defer pool.Close()

	if err := db.Migrate(ctx, pool); err != nil {
		log.Fatalf("migrations: %v", err)
	}

	store := db.NewStore(pool)
	h := handlers.New(service.NewShowService(store), service.NewReservationService(store))

	srv := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: router.New(h, cfg.AdminKey),
	}

	go func() {
		log.Printf("listening on :%s", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server error: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down")

	// Let in-flight requests (e.g. a reserve transaction) finish before exiting.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("forced shutdown: %v", err)
	}
}
