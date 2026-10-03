package handlers

import (
	"github.com/dhruvmehta/seatlock/internal/db"
	"github.com/dhruvmehta/seatlock/internal/metrics"
	"github.com/dhruvmehta/seatlock/internal/service"
)

type Handlers struct {
	store        *db.Store
	shows        *service.ShowService
	reservations *service.ReservationService
	metrics      *metrics.Metrics
	commit       string
}

func New(store *db.Store, shows *service.ShowService, reservations *service.ReservationService, m *metrics.Metrics, commit string) *Handlers {
	return &Handlers{store: store, shows: shows, reservations: reservations, metrics: m, commit: commit}
}
