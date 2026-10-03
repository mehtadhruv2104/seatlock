package handlers

import (
	"bufio"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// TestDecodeJSONBodyFailures sends raw HTTP over TCP to a real server with a
// short ReadTimeout, so a body can genuinely stall or be cut off mid-way.
func TestDecodeJSONBodyFailures(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/x", func(c *gin.Context) {
		var v map[string]any
		if decodeJSON(c, &v) {
			c.Status(http.StatusNoContent)
		}
	})
	srv := httptest.NewUnstartedServer(r)
	srv.Config.ReadTimeout = 300 * time.Millisecond
	srv.Start()
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	tests := []struct {
		name       string
		declared   int    // Content-Length header
		sent       string // body bytes actually sent
		closeWrite bool   // half-close after sending (body cut off)
		wantStatus int
		wantReason string
	}{
		{"valid", 9, `{"a":"b"}`, false, http.StatusNoContent, ""},
		{"malformed JSON", 6, `{"a":}`, false, http.StatusBadRequest, "validation_error"},
		{"body stalls past ReadTimeout", 100, `{"a":`, false, http.StatusRequestTimeout, "request_timeout"},
		{"body cut off mid-way", 100, `{"a":`, true, http.StatusBadRequest, "incomplete_body"},
		{"JSON itself incomplete", 5, `{"a":`, false, http.StatusBadRequest, "incomplete_body"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn, err := net.Dial("tcp", addr)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			req := "POST /x HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\n" +
				"Content-Length: " + itoa(tt.declared) + "\r\n\r\n" + tt.sent
			if _, err := conn.Write([]byte(req)); err != nil {
				t.Fatal(err)
			}
			if tt.closeWrite {
				conn.(*net.TCPConn).CloseWrite()
			}
			conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil {
				t.Fatalf("reading response: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			if tt.wantReason != "" {
				var body map[string]any
				if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
					t.Fatalf("decoding body: %v", err)
				}
				if body["reason"] != tt.wantReason {
					t.Fatalf("reason = %v, want %s (message: %v)", body["reason"], tt.wantReason, body["message"])
				}
			}
		})
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
