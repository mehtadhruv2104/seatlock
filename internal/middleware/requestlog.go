package middleware

import (
	"log/slog"
	"time"
	"unicode"

	"github.com/dhruvmehta/seatlock/internal/logging"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const (
	RequestIDHeader = "X-Request-Id"
	// ReasonKey is set by handlers on error responses so the access log
	// records why a request was declined.
	ReasonKey = "reason"
	// ShowIDKey and ReservationIDKey are set by handlers so per-show and
	// per-reservation detail lives in the logs (metrics stay aggregate-only).
	ShowIDKey        = "show_id"
	ReservationIDKey = "reservation_id"
)

// RequestLog assigns a request id (reusing a caller-supplied X-Request-Id if
// it's sane), echoes it in the response, attaches a logger carrying it to the
// request context, and writes one JSON access-log line per request.
func RequestLog() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		id := c.GetHeader(RequestIDHeader)
		if !validRequestID(id) {
			id = uuid.NewString()
		}
		c.Header(RequestIDHeader, id)

		logger := slog.Default().With("request_id", id)
		c.Request = c.Request.WithContext(logging.WithLogger(c.Request.Context(), logger))

		c.Next()

		status := c.Writer.Status()
		attrs := []any{
			"method", c.Request.Method,
			"route", c.FullPath(),
			"path", c.Request.URL.Path,
			"status", status,
			"latency_ms", float64(time.Since(start).Microseconds()) / 1000,
		}
		if user := UserID(c); user != "" {
			attrs = append(attrs, "user_id", user)
		}
		for _, key := range []string{ShowIDKey, ReservationIDKey, ReasonKey} {
			if v := c.GetString(key); v != "" {
				attrs = append(attrs, key, v)
			}
		}
		if status >= 500 {
			logger.Error("request", attrs...)
		} else {
			logger.Info("request", attrs...)
		}
	}
}

func validRequestID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if r > unicode.MaxASCII || !unicode.IsPrint(r) || r == ' ' {
			return false
		}
	}
	return true
}
