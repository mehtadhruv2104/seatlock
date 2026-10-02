package handlers

import "github.com/dhruvmehta/seatlock/internal/service"

type Handlers struct {
	shows        *service.ShowService
	reservations *service.ReservationService
}

func New(shows *service.ShowService, reservations *service.ReservationService) *Handlers {
	return &Handlers{shows: shows, reservations: reservations}
}
