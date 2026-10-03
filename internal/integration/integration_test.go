// Package integration runs the concurrency guarantees against a real Postgres:
// row locks, unique constraints and isolation can't be tested with mocks.
//
//	make test-integration                          # starts a throwaway Postgres
//	TEST_DATABASE_URL=postgres://... go test -race ./internal/integration/
//
// Without TEST_DATABASE_URL the tests are skipped, so `go test ./...` passes on
// a fresh clone.
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/dhruvmehta/seatlock/internal/db"
	"github.com/dhruvmehta/seatlock/internal/handlers"
	"github.com/dhruvmehta/seatlock/internal/metrics"
	"github.com/dhruvmehta/seatlock/internal/router"
	"github.com/dhruvmehta/seatlock/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	pool         *pgxpool.Pool
	store        *db.Store
	shows        *service.ShowService
	reservations *service.ReservationService
)

func TestMain(m *testing.M) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		fmt.Println("integration tests skipped: TEST_DATABASE_URL not set (run `make test-integration`)")
		os.Exit(0)
	}
	ctx := context.Background()
	var err error
	if pool, err = db.Connect(ctx, url, 32); err != nil {
		fmt.Println("connect:", err)
		os.Exit(1)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		fmt.Println("migrate:", err)
		os.Exit(1)
	}
	store = db.NewStore(pool)
	shows = service.NewShowService(store)
	reservations = service.NewReservationService(store)
	code := m.Run()
	pool.Close()
	os.Exit(code)
}

// ---------------------------------------------------------------- helpers

func newShow(t *testing.T, seats []string, limit int) string {
	t.Helper()
	detail, err := shows.CreateShow(context.Background(), service.CreateShowInput{
		Name: t.Name(), Seats: seats, PricePaise: 25000, PerUserLimit: &limit,
	})
	if err != nil {
		t.Fatalf("create show: %v", err)
	}
	return detail.ID
}

func labels(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s%d", prefix, i+1)
	}
	return out
}

func reserve(user, showID, key string, seats ...string) (service.ReserveResult, error) {
	return reservations.Reserve(context.Background(), service.ReserveInput{
		ShowID: showID, UserID: user, IdempotencyKey: key, Seats: seats,
	})
}

func declineReason(err error) string {
	var de *service.DeclineError
	if errors.As(err, &de) {
		return de.Reason
	}
	return ""
}

// parallel runs fn(i) for i in [0, n) at the same instant and waits.
func parallel(n int, fn func(i int)) {
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			fn(i)
		}(i)
	}
	close(start)
	wg.Wait()
}

// assertConsistent checks the database-level invariants for a show:
// available + held + confirmed == total, every confirmed seat belongs to a
// confirmed reservation that lists it, and every confirmed reservation owns
// exactly the seats it lists.
func assertConsistent(t *testing.T, showID string) {
	t.Helper()
	ctx := context.Background()
	var available, held, confirmed, total int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE status = 'available'), count(*) FILTER (WHERE status = 'held'),
		       count(*) FILTER (WHERE status = 'confirmed'), count(*)
		FROM seats WHERE show_id = $1`, showID).Scan(&available, &held, &confirmed, &total); err != nil {
		t.Fatal(err)
	}
	if available+held+confirmed != total {
		t.Errorf("invariant broken: %d + %d + %d != %d", available, held, confirmed, total)
	}
	var orphans int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM seats s
		LEFT JOIN reservations r ON r.id = s.reservation_id
		WHERE s.show_id = $1 AND s.status = 'confirmed'
		  AND (r.id IS NULL OR r.status <> 'confirmed' OR r.user_id <> s.user_id OR NOT s.seat_label = ANY(r.seats))`,
		showID).Scan(&orphans); err != nil {
		t.Fatal(err)
	}
	if orphans != 0 {
		t.Errorf("%d confirmed seats don't match a confirmed reservation", orphans)
	}
	var mismatched int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM reservations r
		WHERE r.show_id = $1 AND r.status = 'confirmed'
		  AND cardinality(r.seats) <> (SELECT count(*) FROM seats s WHERE s.reservation_id = r.id)`,
		showID).Scan(&mismatched); err != nil {
		t.Fatal(err)
	}
	if mismatched != 0 {
		t.Errorf("%d confirmed reservations don't own exactly the seats they list", mismatched)
	}
}

// ---------------------------------------------------------------- tests

// Correctness bar 1: a race for one seat has exactly one winner; everyone
// else gets a clean seat_taken decline, never an error.
func TestHotSeatStorm(t *testing.T) {
	showID := newShow(t, labels("A", 20), 4)
	const n = 500
	var mu sync.Mutex
	wins, taken, other := 0, 0, []error{}
	parallel(n, func(i int) {
		_, err := reserve(fmt.Sprintf("u%d", i), showID, uuid.NewString(), "A12")
		mu.Lock()
		defer mu.Unlock()
		switch {
		case err == nil:
			wins++
		case declineReason(err) == service.ReasonSeatTaken:
			taken++
		default:
			other = append(other, err)
		}
	})
	if wins != 1 || taken != n-1 || len(other) != 0 {
		t.Fatalf("wins=%d seat_taken=%d other=%v; want 1, %d, none", wins, taken, other, n-1)
	}
	assertConsistent(t, showID)
}

// Correctness bars 1 and 2: overlapping multi-seat requests never deadlock
// (sorted lock order) and never double-sell.
func TestOverlappingMultiSeatNoDeadlock(t *testing.T) {
	seats := labels("D", 8)
	showID := newShow(t, seats, 4)
	var mu sync.Mutex
	var unexpected []error
	parallel(300, func(i int) {
		r := rand.New(rand.NewSource(int64(i)))
		pick := r.Perm(len(seats))[:2+r.Intn(2)]
		want := make([]string, len(pick))
		for k, p := range pick {
			want[k] = seats[p]
		}
		_, err := reserve(fmt.Sprintf("o%d", i), showID, uuid.NewString(), want...)
		if err != nil && declineReason(err) != service.ReasonSeatTaken {
			mu.Lock()
			unexpected = append(unexpected, err)
			mu.Unlock()
		}
	})
	if len(unexpected) > 0 {
		t.Fatalf("%d unexpected errors (deadlock or 5xx-class), first: %v", len(unexpected), unexpected[0])
	}
	assertConsistent(t, showID)
}

// Correctness bar 5: 10 parallel requests from one user on a limit-4 show
// end with exactly 4 seats.
func TestPerUserLimitUnderConcurrency(t *testing.T) {
	seats := labels("B", 10)
	showID := newShow(t, seats, 4)
	var mu sync.Mutex
	booked, limited := 0, 0
	parallel(10, func(i int) {
		_, err := reserve("greedy", showID, uuid.NewString(), seats[i])
		mu.Lock()
		defer mu.Unlock()
		switch {
		case err == nil:
			booked++
		case declineReason(err) == service.ReasonPerUserLimit:
			limited++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	})
	if booked != 4 || limited != 6 {
		t.Fatalf("booked=%d per_user_limit=%d; want 4 and 6", booked, limited)
	}
	assertConsistent(t, showID)
}

// Correctness bar 4: concurrent retries with the same key create exactly one
// reservation; the rest replay it.
func TestSameKeyRaceBooksOnce(t *testing.T) {
	showID := newShow(t, labels("C", 5), 4)
	key := uuid.NewString()
	var mu sync.Mutex
	created, replays := 0, 0
	ids := map[string]bool{}
	parallel(20, func(int) {
		res, err := reserve("racer", showID, key, "C1", "C2")
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			t.Errorf("unexpected error: %v", err)
			return
		}
		ids[res.Reservation.ID] = true
		if res.Replayed {
			replays++
		} else {
			created++
		}
	})
	if created != 1 || replays != 19 || len(ids) != 1 {
		t.Fatalf("created=%d replays=%d distinct ids=%d; want 1, 19, 1", created, replays, len(ids))
	}
	var rows int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM reservations WHERE user_id = 'racer' AND idempotency_key = $1`, key).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("%d reservation rows for one key, want 1", rows)
	}
	assertConsistent(t, showID)
}

// Correctness bar 4: same key, different seats is a conflict, and the order
// of seats in a retry doesn't matter.
func TestSameKeyDifferentSeatsConflicts(t *testing.T) {
	showID := newShow(t, labels("K", 5), 4)
	key := uuid.NewString()
	first, err := reserve("alice", showID, key, "K2", "K1")
	if err != nil {
		t.Fatal(err)
	}
	replay, err := reserve("alice", showID, key, "K1", "K2")
	if err != nil || !replay.Replayed || replay.Reservation.ID != first.Reservation.ID {
		t.Fatalf("reordered retry: replayed=%v id=%s err=%v; want replay of %s", replay.Replayed, replay.Reservation.ID, err, first.Reservation.ID)
	}
	if _, err := reserve("alice", showID, key, "K3"); declineReason(err) != service.ReasonIdempotencyConflict {
		t.Fatalf("different seats on same key: %v; want idempotent_replay_conflict", err)
	}
	assertConsistent(t, showID)
}

// Correctness bar 6 and release: only the owner can cancel; a released seat
// is cleanly re-bookable by exactly one of several racers.
func TestCancelOwnershipAndRebook(t *testing.T) {
	showID := newShow(t, labels("E", 5), 4)
	res, err := reserve("owner", showID, uuid.NewString(), "E1", "E2")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := reservations.Cancel(ctx, "intruder", res.Reservation.ID); !errors.Is(err, service.ErrReservationNotFound) {
		t.Fatalf("cancel by another user: %v; want not found", err)
	}
	if _, err := reservations.Cancel(ctx, "owner", res.Reservation.ID); err != nil {
		t.Fatalf("owner cancel: %v", err)
	}
	again, err := reservations.Cancel(ctx, "owner", res.Reservation.ID)
	if err != nil || !again.AlreadyCancelled {
		t.Fatalf("second cancel: already=%v err=%v; want already cancelled", again.AlreadyCancelled, err)
	}
	var mu sync.Mutex
	winners := 0
	parallel(5, func(i int) {
		if _, err := reserve(fmt.Sprintf("r%d", i), showID, uuid.NewString(), "E1"); err == nil {
			mu.Lock()
			winners++
			mu.Unlock()
		}
	})
	if winners != 1 {
		t.Fatalf("%d racers re-booked released seat E1, want 1", winners)
	}
	assertConsistent(t, showID)
}

// Cancels racing new reservations on the same seats: no deadlocks between the
// two paths (both lock seats in label order), and the data stays consistent.
func TestCancelReserveChurn(t *testing.T) {
	seats := labels("F", 10)
	showID := newShow(t, seats, 4)
	var owned []service.ReserveResult
	for i := 0; i < 5; i++ {
		res, err := reserve(fmt.Sprintf("own%d", i), showID, uuid.NewString(), seats[2*i], seats[2*i+1])
		if err != nil {
			t.Fatal(err)
		}
		owned = append(owned, res)
	}
	var mu sync.Mutex
	var unexpected []error
	parallel(205, func(i int) {
		var err error
		if i < 5 {
			_, err = reservations.Cancel(context.Background(), owned[i].Reservation.UserID, owned[i].Reservation.ID)
		} else {
			r := rand.New(rand.NewSource(int64(i)))
			_, err = reserve(fmt.Sprintf("x%d", i), showID, uuid.NewString(), seats[r.Intn(10)], seats[r.Intn(10)])
			if declineReason(err) != "" {
				err = nil
			}
			var ve *service.ValidationError
			if errors.As(err, &ve) { // the random pick can repeat a seat
				err = nil
			}
		}
		if err != nil {
			mu.Lock()
			unexpected = append(unexpected, err)
			mu.Unlock()
		}
	})
	if len(unexpected) > 0 {
		t.Fatalf("%d unexpected errors, first: %v", len(unexpected), unexpected[0])
	}
	assertConsistent(t, showID)
}

// Correctness bar 6, end to end over HTTP: a user_id in the body is ignored;
// the booking belongs to the token's user.
func TestSpoofedUserIDIgnoredOverHTTP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	m := metrics.New(store)
	srv := httptest.NewServer(router.New(handlers.New(store, shows, reservations, m, "test"), m, "admin"))
	defer srv.Close()

	showID := newShow(t, labels("G", 3), 4)
	body := fmt.Sprintf(`{"seats":["G1"],"idempotency_key":%q,"user_id":"victim"}`, uuid.NewString())
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/shows/"+showID+"/reserve", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer attacker")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusCreated || got["user_id"] != "attacker" {
		t.Fatalf("status=%d user_id=%v; want 201 booked as attacker", resp.StatusCode, got["user_id"])
	}
	var owner string
	if err := pool.QueryRow(context.Background(),
		`SELECT user_id FROM seats WHERE show_id = $1 AND seat_label = 'G1'`, showID).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner != "attacker" {
		t.Fatalf("seat owned by %q in the database, want attacker", owner)
	}
}
