package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestRequireAdmin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/shows", RequireAdmin("s3cret"), func(c *gin.Context) { c.Status(http.StatusCreated) })

	tests := []struct {
		name     string
		key      string
		wantCode int
	}{
		{"correct key", "s3cret", http.StatusCreated},
		{"missing key", "", http.StatusUnauthorized},
		{"wrong key", "nope", http.StatusUnauthorized},
		{"prefix of key", "s3cre", http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/shows", nil)
			if tt.key != "" {
				req.Header.Set("X-Admin-Key", tt.key)
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d", w.Code, tt.wantCode)
			}
		})
	}
}
