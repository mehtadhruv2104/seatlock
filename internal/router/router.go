package router

import (
	"io"

	"github.com/dhruvmehta/seatlock/internal/handlers"
	"github.com/dhruvmehta/seatlock/internal/middleware"
	"github.com/gin-gonic/gin"
)

func New(h *handlers.Handlers, adminKey string) *gin.Engine {
	r := gin.New()
	r.HandleMethodNotAllowed = true
	// RequestLog is outermost so it also logs requests that panicked (as 500).
	// The recovery writer is discarded: handlers.Recover logs the panic as JSON.
	r.Use(
		middleware.RequestLog(),
		gin.CustomRecoveryWithWriter(io.Discard, handlers.Recover),
		middleware.BodyLimit(),
	)
	r.NoRoute(handlers.NotFound)
	r.NoMethod(handlers.MethodNotAllowed)

	r.GET("/healthz", handlers.Healthz)
	r.GET("/readyz", h.Readyz)

	r.POST("/shows", middleware.RequireAdmin(adminKey), h.CreateShow)
	r.GET("/shows/:id", h.GetShow)

	user := r.Group("/", middleware.RequireUser())
	user.POST("/shows/:id/reserve", h.Reserve)
	user.POST("/reservations/:id/cancel", h.Cancel)

	return r
}
