package model

import "time"

type ReservationStatus string

const (
	ReservationConfirmed ReservationStatus = "confirmed"
	ReservationCancelled ReservationStatus = "cancelled"
)

type Reservation struct {
	ID          string            `json:"reservation_id"`
	ShowID      string            `json:"show_id"`
	UserID      string            `json:"user_id"`
	Seats       []string          `json:"seats"`
	AmountPaise int64             `json:"amount_paise"`
	Status      ReservationStatus `json:"status"`
	CancelledAt *time.Time        `json:"cancelled_at,omitempty"`
}
