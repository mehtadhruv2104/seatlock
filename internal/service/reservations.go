package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/dhruvmehta/seatlock/internal/db"
	"github.com/dhruvmehta/seatlock/internal/logging"
	"github.com/dhruvmehta/seatlock/internal/model"
)

type ReservationService struct {
	store *db.Store
}

func NewReservationService(store *db.Store) *ReservationService {
	return &ReservationService{store: store}
}

type ReserveInput struct {
	ShowID         string
	UserID         string
	IdempotencyKey string
	Seats          []string
}

type ReserveResult struct {
	Reservation model.Reservation
	Replayed    bool // true if this key already had a reservation; nothing new was booked
}

// Reserve implements DESIGN.md §4: one transaction that takes locks in the
// fixed order reservation claim -> per-user lock -> seats (sorted). All
// requested seats are confirmed, or none are.
func (s *ReservationService) Reserve(ctx context.Context, in ReserveInput) (ReserveResult, error) {
	if len(in.Seats) == 0 {
		return ReserveResult{}, &ValidationError{Field: "seats", Message: "seats must contain at least one seat label."}
	}
	if dups := duplicates(in.Seats); len(dups) > 0 {
		return ReserveResult{}, &ValidationError{
			Field:   "seats",
			Message: "Each seat may appear only once in a request.",
			Details: map[string]any{"duplicate_seats": dups},
		}
	}
	// Canonical order: used for storage, idempotency comparison and lock order.
	labels := slices.Clone(in.Seats)
	slices.Sort(labels)
	n := len(labels)

	// Step 0: one unlocked read before the transaction. A typo gets a 400 even
	// if the user is also at their limit; validation errors come before
	// domain declines.
	pc, err := s.store.GetReservePrecheck(ctx, in.ShowID, labels, in.UserID, in.IdempotencyKey)
	if errors.Is(err, db.ErrNotFound) {
		return ReserveResult{}, ErrShowNotFound
	}
	if err != nil {
		return ReserveResult{}, err
	}
	if len(pc.Seats) < n {
		found := make([]string, len(pc.Seats))
		for i, seat := range pc.Seats {
			found[i] = seat.Label
		}
		return ReserveResult{}, unknownSeats(labels, found)
	}
	show := pc.Show
	limit := show.PerUserLimit

	// A request this large can never succeed, whatever the user already holds.
	if n > limit {
		return ReserveResult{}, &DeclineError{
			Reason:  ReasonPerUserLimit,
			Message: fmt.Sprintf("You can reserve at most %d seats for this show; this request asks for %d.", limit, n),
			Details: map[string]any{"limit": limit, "requested": n},
		}
	}

	// Pre-check (DESIGN.md §11 P2): a fresh request for a seat that is already
	// sold is declined here, without a transaction, lock or write. This only
	// ever declines, never grants, so a stale read can't cause a double-sell:
	// "available" here is re-checked under lock below. It's skipped when the
	// key is already used, so retries still replay (200) and reused keys still
	// get idempotent_replay_conflict from the transaction.
	if !pc.KeyUsed {
		var taken []string
		for _, seat := range pc.Seats {
			if seat.Status != model.SeatAvailable {
				taken = append(taken, seat.Label)
			}
		}
		if len(taken) > 0 {
			slices.Sort(taken)
			return ReserveResult{}, seatTakenDecline(taken)
		}
	}

	var result ReserveResult
	err = s.store.WithTx(ctx, func(tx *db.Tx) error {
		// Step 1: claim the idempotency key. Must come before the limit check so
		// a retry of a successful booking returns the original, not a 409.
		reservationID, claimed, err := tx.ClaimReservation(ctx, show.ID, in.UserID, in.IdempotencyKey, labels)
		if err != nil {
			return err
		}
		if !claimed {
			existing, err := tx.GetReservationByKey(ctx, in.UserID, in.IdempotencyKey)
			if err != nil {
				return err
			}
			if existing.ShowID == show.ID && slices.Equal(existing.Seats, labels) {
				result = ReserveResult{Reservation: existing, Replayed: true}
				return nil
			}
			return idempotencyConflict(existing, show.ID)
		}

		// Step 2: per-user lock, then count. The count relies on READ COMMITTED:
		// each statement sees everything committed before it started, including
		// this user's bookings that committed while we waited for the lock. Under
		// REPEATABLE READ the count would come from an older snapshot and two
		// parallel requests could both pass the limit.
		if err := tx.LockUser(ctx, show.ID, in.UserID); err != nil {
			return err
		}
		already, err := tx.CountConfirmedSeats(ctx, show.ID, in.UserID)
		if err != nil {
			return err
		}
		if already+n > limit {
			return perUserLimitDecline(limit, already, n)
		}

		// Step 3: lock the requested seats in sorted order.
		locked, err := tx.LockSeats(ctx, show.ID, labels)
		if err != nil {
			return err
		}
		if len(locked) < n { // can't happen (step 0 checked existence); kept as a backstop
			lockedLabels := make([]string, len(locked))
			for i, seat := range locked {
				lockedLabels[i] = seat.Label
			}
			return unknownSeats(labels, lockedLabels)
		}
		var unavailable []string
		seatIDs := make([]string, 0, n)
		for _, seat := range locked {
			if seat.Status != model.SeatAvailable {
				unavailable = append(unavailable, seat.Label)
			}
			seatIDs = append(seatIDs, seat.ID)
		}
		if len(unavailable) > 0 {
			return seatTakenDecline(unavailable)
		}

		// Step 4: confirm. The UPDATE is state-guarded; with the locks above it
		// always affects n rows. If it doesn't, the locking logic has a bug, and
		// we decline rather than risk a double-sell.
		affected, err := tx.ConfirmSeats(ctx, seatIDs, in.UserID, reservationID)
		if err != nil {
			return err
		}
		if affected != int64(n) {
			logging.FromContext(ctx).Error("safeguard tripped: locked seats were not all confirmed",
				"show_id", show.ID, "user_id", in.UserID, "seats", labels, "confirmed", affected, "expected", n)
			return &DeclineError{
				Reason:           ReasonSeatTaken,
				Message:          "Some of these seats are no longer available. No seats were reserved.",
				Details:          map[string]any{"unavailable_seats": labels},
				SafeguardTripped: true,
			}
		}

		amount := show.PricePaise * int64(n)
		if err := tx.SetReservationAmount(ctx, reservationID, amount); err != nil {
			return err
		}
		result = ReserveResult{Reservation: model.Reservation{
			ID:          reservationID,
			ShowID:      show.ID,
			UserID:      in.UserID,
			Seats:       labels,
			AmountPaise: amount,
			Status:      model.ReservationConfirmed,
		}}
		return nil
	})
	if err != nil {
		return ReserveResult{}, err
	}
	return result, nil
}

func seatTakenDecline(unavailable []string) error {
	return &DeclineError{
		Reason:  ReasonSeatTaken,
		Message: fmt.Sprintf("%s no longer available. No seats were reserved.", seatPhrase(unavailable)),
		Details: map[string]any{"unavailable_seats": unavailable},
	}
}

func idempotencyConflict(existing model.Reservation, showID string) error {
	msg := fmt.Sprintf("This idempotency key was already used to reserve %v. Use a new key for a different request.",
		existing.Seats)
	if existing.ShowID != showID {
		msg = "This idempotency key was already used for a reservation on a different show. Use a new key for a different request."
	}
	return &DeclineError{
		Reason:  ReasonIdempotencyConflict,
		Message: msg,
		Details: map[string]any{
			"reservation_id":   existing.ID,
			"original_show_id": existing.ShowID,
			"original_seats":   existing.Seats,
		},
	}
}

func perUserLimitDecline(limit, already, requested int) error {
	remaining := max(limit-already, 0)
	msg := fmt.Sprintf("You can reserve %d more seat(s) for this show (limit %d); this request asks for %d.",
		remaining, limit, requested)
	if remaining == 0 {
		msg = fmt.Sprintf("You already hold %d seats for this show, the maximum allowed.", already)
	}
	return &DeclineError{
		Reason:  ReasonPerUserLimit,
		Message: msg,
		Details: map[string]any{
			"limit":            limit,
			"already_reserved": already,
			"remaining":        remaining,
			"requested":        requested,
		},
	}
}

func unknownSeats(requested, found []string) error {
	exists := make(map[string]bool, len(found))
	for _, label := range found {
		exists[label] = true
	}
	var unknown []string
	for _, label := range requested {
		if !exists[label] {
			unknown = append(unknown, label)
		}
	}
	return &ValidationError{
		Field:   "seats",
		Message: fmt.Sprintf("%s not exist in this show (labels are case-sensitive).", seatPhraseDo(unknown)),
		Details: map[string]any{"unknown_seats": unknown},
	}
}

func duplicates(labels []string) []string {
	seen := make(map[string]int, len(labels))
	var dups []string
	for _, l := range labels {
		seen[l]++
		if seen[l] == 2 {
			dups = append(dups, l)
		}
	}
	return dups
}

// seatPhrase renders "Seat A13 is" / "Seats A12, A13 are".
func seatPhrase(labels []string) string {
	if len(labels) == 1 {
		return "Seat " + labels[0] + " is"
	}
	return "Seats " + strings.Join(labels, ", ") + " are"
}

// seatPhraseDo renders "Seat Z9 does" / "Seats Z9, Z10 do".
func seatPhraseDo(labels []string) string {
	if len(labels) == 1 {
		return "Seat " + labels[0] + " does"
	}
	return "Seats " + strings.Join(labels, ", ") + " do"
}

type CancelResult struct {
	Reservation      model.Reservation
	AlreadyCancelled bool
}

// Cancel releases a reservation's seats (DESIGN.md §6). Locks: reservation
// row, then its seats in sorted order, consistent with reserve's lock order.
// Cancelling twice is harmless and returns the cancelled reservation. A
// reservation that isn't the caller's is reported as not found, so its
// existence isn't revealed.
func (s *ReservationService) Cancel(ctx context.Context, userID, reservationID string) (CancelResult, error) {
	var result CancelResult
	err := s.store.WithTx(ctx, func(tx *db.Tx) error {
		r, err := tx.LockReservation(ctx, reservationID)
		if errors.Is(err, db.ErrNotFound) {
			return ErrReservationNotFound
		}
		if err != nil {
			return err
		}
		if r.UserID != userID {
			return ErrReservationNotFound
		}
		if r.Status == model.ReservationCancelled {
			result = CancelResult{Reservation: r, AlreadyCancelled: true}
			return nil
		}

		locked, err := tx.LockReservationSeats(ctx, r.ID)
		if err != nil {
			return err
		}
		released, err := tx.ReleaseSeats(ctx, r.ID)
		if err != nil {
			return err
		}
		if released != int64(len(r.Seats)) || int(released) != locked {
			logging.FromContext(ctx).Error("cancel mismatch: seats released differ from reservation",
				"reservation_id", r.ID, "listed", len(r.Seats), "locked", locked, "released", released)
		}

		at, err := tx.MarkCancelled(ctx, r.ID)
		if err != nil {
			return err
		}
		r.Status = model.ReservationCancelled
		r.CancelledAt = &at
		result = CancelResult{Reservation: r}
		return nil
	})
	if err != nil {
		return CancelResult{}, err
	}
	return result, nil
}
