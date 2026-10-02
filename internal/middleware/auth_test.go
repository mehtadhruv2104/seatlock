package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestRequireUser(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/me", RequireUser(), func(c *gin.Context) {
		c.String(http.StatusOK, UserID(c))
	})

	tests := []struct {
		name     string
		header   string
		wantCode int
		wantUser string
	}{
		{"valid token", "Bearer alice", http.StatusOK, "alice"},
		{"scheme is case-insensitive", "bearer alice", http.StatusOK, "alice"},
		{"surrounding spaces trimmed", "Bearer   alice  ", http.StatusOK, "alice"},
		{"no header", "", http.StatusUnauthorized, ""},
		{"wrong scheme", "Basic YWxpY2U=", http.StatusUnauthorized, ""},
		{"empty token", "Bearer ", http.StatusUnauthorized, ""},
		{"space inside token", "Bearer al ice", http.StatusUnauthorized, ""},
		{"token too long", "Bearer " + strings.Repeat("a", 129), http.StatusUnauthorized, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/me", nil)
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, tt.wantCode, w.Body)
			}
			if tt.wantCode == http.StatusOK && w.Body.String() != tt.wantUser {
				t.Fatalf("user = %q, want %q", w.Body.String(), tt.wantUser)
			}
			if tt.wantCode == http.StatusUnauthorized && w.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Fatal("missing WWW-Authenticate header on 401")
			}
		})
	}
}
