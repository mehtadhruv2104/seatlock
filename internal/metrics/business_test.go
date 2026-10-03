package metrics

import (
	"context"
	"errors"
	"testing"

	"github.com/dhruvmehta/seatlock/internal/db"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestBusinessCounters(t *testing.T) {
	m := &Metrics{biz: newBusiness()}

	for _, r := range declineReasons {
		if got := testutil.ToFloat64(m.biz.declined.WithLabelValues(r)); got != 0 {
			t.Fatalf("reason %q should start at 0, got %v", r, got)
		}
	}

	m.ReservationConfirmed(2)
	m.ReservationConfirmed(1)
	m.Declined("seat_taken")
	m.Declined(ReasonIdempotentReplay)
	m.ReservationCancelled(2)

	checks := map[string]struct {
		c    prometheus.Collector
		want float64
	}{
		"reservations confirmed": {m.biz.reservationsConfirmed, 2},
		"seats confirmed":        {m.biz.seatsConfirmed, 3},
		"declined seat_taken":    {m.biz.declined.WithLabelValues("seat_taken"), 1},
		"declined replay":        {m.biz.declined.WithLabelValues(ReasonIdempotentReplay), 1},
		"reservations cancelled": {m.biz.reservationsCancelled, 1},
		"seats released":         {m.biz.seatsReleased, 2},
		"safeguard trips":        {m.biz.safeguardTrips, 0},
	}
	for name, ch := range checks {
		if got := testutil.ToFloat64(ch.c); got != ch.want {
			t.Errorf("%s = %v, want %v", name, got, ch.want)
		}
	}
}

type fakeTotals struct {
	t   db.SeatTotals
	err error
}

func (f fakeTotals) SeatTotals(context.Context) (db.SeatTotals, error) { return f.t, f.err }

func TestSeatCollector(t *testing.T) {
	ok := newSeatCollector(fakeTotals{t: db.SeatTotals{Available: 7, Held: 0, Confirmed: 3, Total: 10}})
	if n := testutil.CollectAndCount(ok); n != 5 {
		t.Fatalf("healthy scrape: %d metrics, want 5 (four gauges + up)", n)
	}

	broken := newSeatCollector(fakeTotals{err: errors.New("db down")})
	if n := testutil.CollectAndCount(broken); n != 1 {
		t.Fatalf("failed scrape: %d metrics, want 1 (only up=0)", n)
	}
	if n := testutil.CollectAndCount(broken, "seatlock_seat_gauges_up"); n != 1 {
		t.Fatal("failed scrape must still report seat_gauges_up")
	}
}
