package router

import (
	"github.com/dhruvmehta/seatlock/internal/handlers"
	"github.com/gin-gonic/gin"
)

func New() *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery())

	r.GET("/healthz", handlers.Healthz)

	return r
}
