package router

import (
	"github.com/dhruvmehta/seatlock/internal/handlers"
	"github.com/gin-gonic/gin"
)

func New(h *handlers.Handlers) *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery())

	r.GET("/healthz", handlers.Healthz)

	r.POST("/shows", h.CreateShow)
	r.GET("/shows/:id", h.GetShow)

	return r
}
