package handlers

import (
	"github.com/dhruvmehta/seatlock/internal/db"
	"github.com/dhruvmehta/seatlock/internal/service"
)

type Handlers struct {
	store        *db.Store
	shows        *service.ShowService
	reservations *service.ReservationService
}

func New(store *db.Store, shows *service.ShowService, reservations *service.ReservationService) *Handlers {
	return &Handlers{store: store, shows: shows, reservations: reservations}
}
