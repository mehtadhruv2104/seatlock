package handlers

import (
	"net/http"
	"time"

	"github.com/dhruvmehta/seatlock/internal/logging"
	"github.com/gin-gonic/gin"
)

const readinessDBTimeout = 2 * time.Second

// Healthz is liveness: the process is up. It deliberately doesn't touch the
// database, so a database outage marks us not-ready rather than getting a
// healthy process restarted.
func Healthz(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// Readyz is readiness: can we serve traffic right now? Fails closed when the
// database doesn't answer within the timeout.
func (h *Handlers) Readyz(c *gin.Context) {
	if err := h.store.Ping(c.Request.Context(), readinessDBTimeout); err != nil {
		logging.FromContext(c.Request.Context()).Error("readiness: database unreachable", "error", err.Error())
		writeError(c, http.StatusServiceUnavailable, "database_unreachable",
			"The database is not reachable.", map[string]any{"status": "not_ready"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ready", "checks": gin.H{"database": "ok"}})
}
