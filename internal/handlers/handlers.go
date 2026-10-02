package handlers

import "github.com/dhruvmehta/seatlock/internal/service"

type Handlers struct {
	shows *service.ShowService
}

func New(shows *service.ShowService) *Handlers {
	return &Handlers{shows: shows}
}
