package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/dhruvmehta/seatlock/internal/db"
	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const namespace = "seatlock"

// Metrics owns a dedicated registry. Every label has a small, fixed set of
// values (route templates, status codes, decline reasons): no per-show or
// per-user labels, so the number of series never grows with traffic
// (DESIGN.md change log, 2026-10-03).
type Metrics struct {
	reg          *prometheus.Registry
	httpRequests *prometheus.CounterVec
	httpDuration *prometheus.HistogramVec
}

func New(store *db.Store) *Metrics {
	m := &Metrics{
		reg: prometheus.NewRegistry(),
		httpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "http_requests_total",
			Help: "HTTP requests by method, route template and status code.",
		}, []string{"method", "route", "status"}),
		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace, Name: "http_request_duration_seconds",
			Help:    "HTTP request latency by method and route template.",
			Buckets: []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		}, []string{"method", "route"}),
	}
	m.reg.MustRegister(
		m.httpRequests,
		m.httpDuration,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	m.registerPoolMetrics(store)
	return m
}

func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// Middleware records every request's count and latency by route template
// (e.g. /shows/:id/reserve), never the raw path.
func (m *Metrics) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		route := c.FullPath()
		if route == "" {
			route = "unmatched"
		}
		method := c.Request.Method
		m.httpRequests.WithLabelValues(method, route, strconv.Itoa(c.Writer.Status())).Inc()
		m.httpDuration.WithLabelValues(method, route).Observe(time.Since(start).Seconds())
	}
}

// registerPoolMetrics exposes pgxpool statistics, read at scrape time. Waits
// for a connection are the signal for the §11 P1/P5 decisions: if requests
// queue for connections during a burst, the pool is the bottleneck.
func (m *Metrics) registerPoolMetrics(store *db.Store) {
	gauge := func(name, help string, f func() float64) prometheus.Collector {
		return prometheus.NewGaugeFunc(prometheus.GaugeOpts{Namespace: namespace, Subsystem: "db_pool", Name: name, Help: help}, f)
	}
	counter := func(name, help string, f func() float64) prometheus.Collector {
		return prometheus.NewCounterFunc(prometheus.CounterOpts{Namespace: namespace, Subsystem: "db_pool", Name: name, Help: help}, f)
	}
	m.reg.MustRegister(
		gauge("acquired_conns", "Connections currently checked out.", func() float64 { return float64(store.PoolStat().AcquiredConns()) }),
		gauge("idle_conns", "Idle connections in the pool.", func() float64 { return float64(store.PoolStat().IdleConns()) }),
		gauge("max_conns", "Maximum pool size.", func() float64 { return float64(store.PoolStat().MaxConns()) }),
		counter("acquires_total", "Connections acquired from the pool.", func() float64 { return float64(store.PoolStat().AcquireCount()) }),
		counter("acquire_waits_total", "Acquires that had to wait because no connection was free.", func() float64 { return float64(store.PoolStat().EmptyAcquireCount()) }),
		counter("acquire_wait_seconds_total", "Total time spent acquiring connections.", func() float64 { return store.PoolStat().AcquireDuration().Seconds() }),
	)
}
