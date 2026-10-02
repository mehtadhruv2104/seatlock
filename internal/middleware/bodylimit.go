package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// MaxBodyBytes fits a 100k-seat show (~1.1 MB of JSON) with plenty of room.
const MaxBodyBytes = 8 << 20

// BodyLimit stops reading a request body past MaxBodyBytes; the JSON decoder
// then reports it and the handler answers 413.
func BodyLimit() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, MaxBodyBytes)
		c.Next()
	}
}
