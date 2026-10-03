package middleware

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// ReserveRoute is the route whose access logs are sampled and summarized.
const ReserveRoute = "/shows/:id/reserve"

// ReserveLog keeps reserve access logs under Railway's 500 lines/s per replica
// limit (above it, lines are dropped at random). Hot-seat losers (409
// seat_taken) are ~95% of a burst and their lines are all alike, so only 1 in
// sampleRate of them is logged. Every reserve outcome is still counted, and a
// summary line per interval accounts for all of them (DESIGN.md change log).
type ReserveLog struct {
	sampleRate int64
	seatTaken  atomic.Int64

	mu     sync.Mutex
	counts map[string]int64
	since  time.Time
}

func NewReserveLog(sampleRate int) *ReserveLog {
	return &ReserveLog{sampleRate: int64(sampleRate), counts: map[string]int64{}, since: time.Now()}
}

// observe counts one reserve response and reports whether to write its access
// log line, and whether that line is a sample.
func (r *ReserveLog) observe(status int, reason string) (write, sampled bool) {
	key := outcomeKey(status, reason)
	r.mu.Lock()
	r.counts[key]++
	r.mu.Unlock()

	if key != "seat_taken" || r.sampleRate <= 1 {
		return true, false
	}
	// Deterministic: the 1st, (rate+1)th, (2*rate+1)th ... seat_taken are logged.
	n := r.seatTaken.Add(1)
	return (n-1)%r.sampleRate == 0, true
}

func outcomeKey(status int, reason string) string {
	switch {
	case status == 201:
		return "confirmed"
	case status == 200:
		return "replay"
	case status == 409 && reason != "":
		return reason
	default:
		return "status_" + strconv.Itoa(status)
	}
}

// Run writes a summary every interval (only when there was traffic) until ctx
// is cancelled, then writes a final one for the last partial interval.
func (r *ReserveLog) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			r.flush()
			return
		case <-t.C:
			r.flush()
		}
	}
}

func (r *ReserveLog) flush() {
	r.mu.Lock()
	counts, since := r.counts, r.since
	r.counts, r.since = map[string]int64{}, time.Now()
	r.mu.Unlock()

	var total int64
	attrs := []any{}
	for k, v := range counts {
		total += v
		attrs = append(attrs, k, v)
	}
	if total == 0 {
		return
	}
	attrs = append([]any{
		"route", ReserveRoute,
		"window_ms", time.Since(since).Milliseconds(),
		"total", total,
		"seat_taken_sample_rate", r.sampleRate,
	}, attrs...)
	slog.Info("reserve summary", attrs...)
}
