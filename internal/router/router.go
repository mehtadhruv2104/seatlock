package router

import (
	"github.com/dhruvmehta/seatlock/internal/handlers"
	"github.com/dhruvmehta/seatlock/internal/middleware"
	"github.com/gin-gonic/gin"
)

func New(h *handlers.Handlers, adminKey string) *gin.Engine {
	r := gin.New()
	r.Use(middleware.RequestLog(), gin.Recovery())

	r.GET("/healthz", handlers.Healthz)

	r.POST("/shows", middleware.RequireAdmin(adminKey), h.CreateShow)
	r.GET("/shows/:id", h.GetShow)

	user := r.Group("/", middleware.RequireUser())
	user.POST("/shows/:id/reserve", h.Reserve)
	user.POST("/reservations/:id/cancel", h.Cancel)

	return r
}
