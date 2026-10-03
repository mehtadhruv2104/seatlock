package middleware

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// captureLogs routes the default slog logger into a buffer for the test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func logLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("bad log line %q: %v", l, err)
		}
		out = append(out, m)
	}
	return out
}

func TestReserveLogSamplingAndSummary(t *testing.T) {
	gin.SetMode(gin.TestMode)
	buf := captureLogs(t)
	rl := NewReserveLog(10)

	r := gin.New()
	r.Use(RequestLog(rl))
	// The test route answers with whatever status/reason the query asks for.
	r.POST(ReserveRoute, func(c *gin.Context) {
		status, _ := strconv.Atoi(c.Query("status"))
		if reason := c.Query("reason"); reason != "" {
			c.Set(ReasonKey, reason)
		}
		c.Status(status)
	})
	r.GET("/healthz", func(c *gin.Context) { c.Status(http.StatusOK) })
	send := func(method, path string) {
		r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(method, path, nil))
	}

	for i := 0; i < 25; i++ {
		send("POST", "/shows/x/reserve?status=409&reason=seat_taken")
	}
	send("POST", "/shows/x/reserve?status=201")
	send("POST", "/shows/x/reserve?status=201")
	send("POST", "/shows/x/reserve?status=200")
	send("POST", "/shows/x/reserve?status=409&reason=per_user_limit")
	send("POST", "/shows/x/reserve?status=400&reason=validation_error")
	send("GET", "/healthz")

	var seatTaken, sampled, others int
	for _, l := range logLines(t, buf) {
		if l["msg"] != "request" {
			continue
		}
		if l["reason"] == "seat_taken" {
			seatTaken++
			if l["sampled"] == true && l["sample_rate"] == float64(10) {
				sampled++
			}
		} else {
			others++
		}
	}
	// 25 seat_taken at 1 in 10: the 1st, 11th and 21st are logged.
	if seatTaken != 3 || sampled != 3 {
		t.Errorf("seat_taken lines = %d (sampled-marked %d), want 3 and 3", seatTaken, sampled)
	}
	// 2 confirmed + replay + per_user_limit + 400 + healthz: never sampled.
	if others != 6 {
		t.Errorf("other request lines = %d, want 6", others)
	}

	buf.Reset()
	rl.flush()
	lines := logLines(t, buf)
	if len(lines) != 1 || lines[0]["msg"] != "reserve summary" {
		t.Fatalf("want one summary line, got %v", lines)
	}
	want := map[string]float64{"total": 30, "seat_taken": 25, "confirmed": 2, "replay": 1, "per_user_limit": 1, "status_400": 1}
	for k, v := range want {
		if lines[0][k] != v {
			t.Errorf("summary %s = %v, want %v", k, lines[0][k], v)
		}
	}

	buf.Reset()
	rl.flush()
	if buf.Len() != 0 {
		t.Errorf("idle flush wrote %q, want nothing", buf.String())
	}
}

func TestReserveLogRateOneLogsEverything(t *testing.T) {
	rl := NewReserveLog(1)
	for i := 0; i < 5; i++ {
		if write, sampled := rl.observe(409, "seat_taken"); !write || sampled {
			t.Fatalf("rate 1: write=%v sampled=%v, want true/false", write, sampled)
		}
	}
}
