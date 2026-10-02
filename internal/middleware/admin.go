package middleware

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"

	"github.com/gin-gonic/gin"
)

// RequireAdmin checks the X-Admin-Key header against the configured key.
// Comparing SHA-256 digests in constant time avoids leaking the key, or its
// length, through response timing.
func RequireAdmin(adminKey string) gin.HandlerFunc {
	want := sha256.Sum256([]byte(adminKey))
	return func(c *gin.Context) {
		got := c.GetHeader("X-Admin-Key")
		if got == "" {
			adminUnauthorized(c, "This endpoint requires the X-Admin-Key header.")
			return
		}
		sum := sha256.Sum256([]byte(got))
		if subtle.ConstantTimeCompare(sum[:], want[:]) != 1 {
			adminUnauthorized(c, "Invalid admin key.")
			return
		}
		c.Next()
	}
}

func adminUnauthorized(c *gin.Context, message string) {
	c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"reason": "unauthorized", "message": message})
}
