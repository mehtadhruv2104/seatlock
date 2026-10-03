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
}

func New(store *db.Store, shows *service.ShowService, reservations *service.ReservationService, m *metrics.Metrics) *Handlers {
	return &Handlers{store: store, shows: shows, reservations: reservations, metrics: m}
}
