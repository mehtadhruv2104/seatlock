package router

import (
	"io"

	"github.com/dhruvmehta/seatlock/internal/handlers"
	"github.com/dhruvmehta/seatlock/internal/metrics"
	"github.com/dhruvmehta/seatlock/internal/middleware"
	"github.com/gin-gonic/gin"
)

func New(h *handlers.Handlers, m *metrics.Metrics, adminKey string) *gin.Engine {
	r := gin.New()
	r.HandleMethodNotAllowed = true
	// Logging and metrics wrap recovery so requests that panicked are still
	// recorded (as 500). The recovery writer is discarded: handlers.Recover
	// logs the panic as JSON.
	r.Use(
		middleware.RequestLog(),
		m.Middleware(),
		gin.CustomRecoveryWithWriter(io.Discard, handlers.Recover),
		middleware.BodyLimit(),
	)
	r.NoRoute(handlers.NotFound)
	r.NoMethod(handlers.MethodNotAllowed)

	r.GET("/healthz", h.Healthz)
	r.GET("/readyz", h.Readyz)
	r.GET("/metrics", gin.WrapH(m.Handler()))

	r.POST("/shows", middleware.RequireAdmin(adminKey), h.CreateShow)
	r.GET("/shows/:id", h.GetShow)

	user := r.Group("/", middleware.RequireUser())
	user.POST("/shows/:id/reserve", h.Reserve)
	user.POST("/reservations/:id/cancel", h.Cancel)

	return r
}
