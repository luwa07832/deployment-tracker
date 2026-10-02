package v1

import (
	"github.com/gin-gonic/gin"

	"github.com/luwa07832/deployment-tracker/internal/store"
)

// Dependencies is everything the v1 router needs from the process.
type Dependencies struct {
	Store *store.Store
}

// Register mounts the traceable release-record routes onto an existing
// engine. All paths live under /api/v1 so baseline endpoints keep their exact
// behavior and JSON contract.
func Register(engine *gin.Engine, deps Dependencies) {
	group := engine.Group("/api/v1")
	group.POST("/environments", registerEnvironment(deps))
	group.GET("/environments", listEnvironments(deps))
	group.GET("/environments/:environment", getEnvironment(deps))
	group.POST("/release-records", createReleaseRecord(deps))
	group.GET("/release-records", listReleaseRecords(deps))
	group.GET("/release-records/:id", getReleaseRecord(deps))
	group.GET("/change-entries", listChangeEntries(deps))
	group.GET("/compare", compareEnvironments(deps))
	group.GET("/release-comparison", compareReleases(deps))
	group.GET("/environments/:environment/release-history", listReleaseHistory(deps))
	group.GET("/release-batches/:batch_id/promotion-chain", getPromotionChain(deps))
	group.GET("/release-batches/:batch_id/promotion-diff", getPromotionDiff(deps))
	group.GET("/release-batches/:batch_id/changes/:title/trace", tracePromotionChange(deps))
	group.GET("/promotion-routes", listPromotionRoutes(deps))
	group.POST("/promotion-routes", createPromotionRoute(deps))
	group.GET("/promotion-routes/:name", getPromotionRoute(deps))
	group.PUT("/release-batches/:batch_id/promotion-route", bindPromotionRoute(deps))
}
