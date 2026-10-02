package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

const (
	userIDKey         = "user_id"
	maxUserTokenLen   = 128
	bearerScheme      = "bearer "
	authHeaderExample = "Authorization: Bearer <user_token>"
)

// RequireUser derives the caller's identity from the bearer token. The token
// itself is the user id: a deliberate testing simplification (DESIGN.md §7);
// production would verify a signed token and extract the id from it.
// Identity never comes from the request body.
func RequireUser() gin.HandlerFunc {
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		if len(header) < len(bearerScheme) || !strings.EqualFold(header[:len(bearerScheme)], bearerScheme) {
			unauthorized(c, "Missing bearer token. Send "+authHeaderExample+".")
			return
		}
		token := strings.TrimSpace(header[len(bearerScheme):])
		if token == "" || len(token) > maxUserTokenLen || strings.ContainsAny(token, " \t") {
			unauthorized(c, "Invalid user_token: it must be 1-128 characters with no spaces.")
			return
		}
		c.Set(userIDKey, token)
		c.Next()
	}
}

// UserID returns the authenticated user. Only valid behind RequireUser.
func UserID(c *gin.Context) string {
	return c.GetString(userIDKey)
}

func unauthorized(c *gin.Context, message string) {
	c.Header("WWW-Authenticate", "Bearer")
	c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"reason": "unauthorized", "message": message})
}
