package model

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
}
