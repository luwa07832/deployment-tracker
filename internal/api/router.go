// Package api holds the HTTP routing layer.
package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	apiv1 "github.com/luwa07832/deployment-tracker/internal/api/v1"
	"github.com/luwa07832/deployment-tracker/internal/store"
)

// Dependencies is everything the router needs from the process around it.
type Dependencies struct {
	Store *store.Store
}

// NewRouter builds the HTTP handler. Only the entry points documented in README.md are registered.
func NewRouter(deps Dependencies) *gin.Engine {
	engine := gin.New()
	engine.Use(gin.Recovery())
	engine.HandleMethodNotAllowed = true
	engine.NoRoute(func(c *gin.Context) { fail(c, http.StatusNotFound, store.CodeNotFound, "no such endpoint") })

	engine.GET("/healthz", healthz(deps))
	engine.POST("/releases", createRelease(deps))
	engine.GET("/releases", listReleases(deps))
	engine.GET("/releases/:environment/:version", getRelease(deps))
	engine.GET("/environments/:environment/history", getHistory(deps))
	engine.GET("/compare", compareEnvironments(deps))
	apiv1.Register(engine, apiv1.Dependencies{Store: deps.Store})
	return engine
}

// healthz reports process and storage health. It never reads or writes deployment records.
func healthz(deps Dependencies) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := deps.Store.Ping(); err != nil {
			fail(c, http.StatusServiceUnavailable, store.CodeStorageUnavailable, "database is not available")
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok", "database": "ok"})
	}
}

// fail writes the single documented error shape.
func fail(c *gin.Context, status int, code, message string) {
	c.AbortWithStatusJSON(status, gin.H{"error": gin.H{"code": code, "message": message}})
}
