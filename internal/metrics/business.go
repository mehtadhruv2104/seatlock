package metrics

import (
	"context"
	"log/slog"
	"time"

	"github.com/dhruvmehta/seatlock/internal/db"
	"github.com/prometheus/client_golang/prometheus"
)

// ReasonIdempotentReplay labels a retry that returned the original
// reservation (200): nothing new was booked, which the spec counts as a
// decline. The 409 reasons come from the service package.
const ReasonIdempotentReplay = "idempotent_replay"

var declineReasons = []string{"seat_taken", "per_user_limit", "idempotent_replay_conflict", ReasonIdempotentReplay}

type business struct {
	reservationsConfirmed prometheus.Counter
	seatsConfirmed        prometheus.Counter
	declined              *prometheus.CounterVec
	reservationsCancelled prometheus.Counter
	seatsReleased         prometheus.Counter
	safeguardTrips        prometheus.Counter
}

func newBusiness() business {
	counter := func(name, help string) prometheus.Counter {
		return prometheus.NewCounter(prometheus.CounterOpts{Namespace: namespace, Name: name, Help: help})
	}
	b := business{
		reservationsConfirmed: counter("reservations_confirmed_total", "Reservations confirmed (201 responses)."),
		seatsConfirmed:        counter("seats_confirmed_total", "Seats confirmed by successful reservations."),
		declined: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "reservations_declined_total",
			Help: "Reserve requests that booked nothing, by reason.",
		}, []string{"reason"}),
		reservationsCancelled: counter("reservations_cancelled_total", "Reservations cancelled by their owner."),
		seatsReleased:         counter("seats_released_total", "Seats returned to available by cancellations."),
		safeguardTrips:        counter("safeguard_trips_total", "Reserve safeguard trips: locked seats not all confirmed. Must stay 0; any increase is a locking bug."),
	}
	// Create every reason at 0 so the series exist before the first decline.
	for _, r := range declineReasons {
		b.declined.WithLabelValues(r)
	}
	return b
}

func (b business) collectors() []prometheus.Collector {
	return []prometheus.Collector{b.reservationsConfirmed, b.seatsConfirmed, b.declined,
		b.reservationsCancelled, b.seatsReleased, b.safeguardTrips}
}

func (m *Metrics) ReservationConfirmed(seats int) {
	m.biz.reservationsConfirmed.Inc()
	m.biz.seatsConfirmed.Add(float64(seats))
}

func (m *Metrics) Declined(reason string) { m.biz.declined.WithLabelValues(reason).Inc() }

func (m *Metrics) ReservationCancelled(seats int) {
	m.biz.reservationsCancelled.Inc()
	m.biz.seatsReleased.Add(float64(seats))
}

func (m *Metrics) SafeguardTripped() { m.biz.safeguardTrips.Inc() }

type seatTotaler interface {
	SeatTotals(ctx context.Context) (db.SeatTotals, error)
}

// seatCollector reads seat counts from the database at scrape time, so the
// gauges are the database state at that moment: they can't drift or go stale.
type seatCollector struct {
	store                             seatTotaler
	available, held, confirmed, total *prometheus.Desc
	up                                *prometheus.Desc
}

func newSeatCollector(store seatTotaler) *seatCollector {
	desc := func(name, help string) *prometheus.Desc {
		return prometheus.NewDesc(prometheus.BuildFQName(namespace, "", name), help, nil, nil)
	}
	return &seatCollector{
		store:     store,
		available: desc("seats_available", "Seats currently available, across all shows."),
		held:      desc("seats_held", "Seats currently held, across all shows."),
		confirmed: desc("seats_confirmed", "Seats currently confirmed, across all shows."),
		total:     desc("seats_total", "All seats across all shows. Equals available + held + confirmed."),
		up:        desc("seat_gauges_up", "1 if the seat gauges were read from the database on this scrape, 0 if it failed."),
	}
}

func (c *seatCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{c.available, c.held, c.confirmed, c.total, c.up} {
		ch <- d
	}
}

func (c *seatCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	t, err := c.store.SeatTotals(ctx)
	if err != nil {
		slog.Error("metrics: reading seat totals failed", "error", err.Error())
		ch <- prometheus.MustNewConstMetric(c.up, prometheus.GaugeValue, 0)
		return
	}
	ch <- prometheus.MustNewConstMetric(c.up, prometheus.GaugeValue, 1)
	ch <- prometheus.MustNewConstMetric(c.available, prometheus.GaugeValue, float64(t.Available))
	ch <- prometheus.MustNewConstMetric(c.held, prometheus.GaugeValue, float64(t.Held))
	ch <- prometheus.MustNewConstMetric(c.confirmed, prometheus.GaugeValue, float64(t.Confirmed))
	ch <- prometheus.MustNewConstMetric(c.total, prometheus.GaugeValue, float64(t.Total))
}
