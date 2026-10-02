package model

import "time"

type SeatStatus string

const (
	SeatAvailable SeatStatus = "available"
	SeatHeld      SeatStatus = "held"
	SeatConfirmed SeatStatus = "confirmed"
)

type Show struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	PricePaise   int64     `json:"price_paise"`
	PerUserLimit int       `json:"per_user_limit"`
	CreatedAt    time.Time `json:"created_at"`
}

// Seat is the public view of a seat: who holds it is deliberately not exposed.
type Seat struct {
	Label  string     `json:"label"`
	Status SeatStatus `json:"status"`
}

type SeatCounts struct {
	Available int `json:"available"`
	Held      int `json:"held"`
	Confirmed int `json:"confirmed"`
	Total     int `json:"total"`
}

type ShowDetail struct {
	Show
	Counts SeatCounts `json:"counts"`
	Seats  []Seat     `json:"seats"`
}
