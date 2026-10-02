package v1

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/luwa07832/deployment-tracker/internal/store"
)

const (
	rollbackTargetResolved  = "resolved"
	rollbackTargetRequested = "requested"
	rollbackTargetMissing   = "missing"
)

type releaseStateView struct {
	Environment          string             `json:"environment"`
	AsOf                 string             `json:"as_of"`
	CurrentRelease       recordView         `json:"current_release"`
	PreviousRelease      *recordView        `json:"previous_release"`
	RollbackTargetStatus string             `json:"rollback_target_status"`
	RollbackTarget       *recordView        `json:"rollback_target"`
	RollbackImpact       rollbackImpactView `json:"rollback_impact"`
}

type rollbackImpactView struct {
	FromVersion   string                  `json:"from_version"`
	ToVersion     string                  `json:"to_version"`
	Version       releaseScalarDiffView   `json:"version"`
	GateStatus    releaseScalarDiffView   `json:"gate_status"`
	RollbackPoint rollbackPointDiffView   `json:"rollback_point"`
	Changes       []releaseChangeDiffView `json:"changes"`
	ChangeSummary releaseChangeTotalsView `json:"change_summary"`
	Consistent    bool                    `json:"consistent"`
}

// getReleaseState reports the effective release of one environment at a point
// in time together with the rollback target and the impact of rolling back to
// it. The endpoint is read-only; an explicit as_of yields a byte-stable
// result while data is unchanged.
func getReleaseState(deps Dependencies) gin.HandlerFunc {
	return func(c *gin.Context) {
		environment := c.Param("environment")
		rawTarget, targetProvided := c.GetQuery("target_version")
		if targetProvided && strings.TrimSpace(rawTarget) == "" {
			fail(c, http.StatusBadRequest, store.CodeInvalidReleaseStateTarget,
				"target_version must not be blank when provided")
			return
		}
		asOf := time.Now().UTC().Format("2006-01-02T15:04:05Z")
		if rawAsOf, present := c.GetQuery("as_of"); present {
			bound, err := parseTimeBound(rawAsOf, true)
			if err != nil {
				fail(c, http.StatusUnprocessableEntity, store.CodeReleaseStateTimeInvalid,
					"as_of must be an RFC3339 timestamp or a YYYY-MM-DD date")
				return
			}
			asOf = bound
		}
		exists, err := deps.Store.TrackedEnvironmentExists(environment)
		if err != nil {
			fail(c, http.StatusServiceUnavailable, store.CodeStorageUnavailable, "database is not available")
			return
		}
		if !exists {
			fail(c, http.StatusNotFound, store.CodeEnvironmentNotFoundV1, "no such environment "+environment)
			return
		}
		records, err := deps.Store.ListReleaseRecordsAt(environment, asOf, 2)
		if err != nil {
			fail(c, http.StatusServiceUnavailable, store.CodeStorageUnavailable, "database is not available")
			return
		}
		if len(records) == 0 {
			fail(c, http.StatusNotFound, store.CodeReleaseStateNotFound,
				"no release recorded for environment "+environment+" at or before "+asOf)
			return
		}
		current := &records[0]
		var previous *store.ReleaseRecord
		if len(records) > 1 {
			previous = &records[1]
		}

		targetStatus := rollbackTargetResolved
		var target *store.ReleaseRecord
		if targetProvided {
			target, err = deps.Store.EffectiveReleaseRecord(environment, rawTarget)
			if err != nil {
				fail(c, http.StatusServiceUnavailable, store.CodeStorageUnavailable, "database is not available")
				return
			}
			if target == nil {
				fail(c, http.StatusNotFound, store.CodeRollbackTargetNotFound,
					"version "+rawTarget+" is not recorded for environment "+environment)
				return
			}
			targetStatus = rollbackTargetRequested
		} else {
			target, err = deps.Store.EffectiveReleaseRecordAt(environment, current.RollbackPoint, asOf)
			if err != nil {
				fail(c, http.StatusServiceUnavailable, store.CodeStorageUnavailable, "database is not available")
				return
			}
			if target == nil {
				targetStatus = rollbackTargetMissing
			}
		}

		view := releaseStateView{
			Environment:          environment,
			AsOf:                 asOf,
			CurrentRelease:       toRecordView(current),
			PreviousRelease:      toOptionalRecordView(previous),
			RollbackTargetStatus: targetStatus,
			RollbackTarget:       toOptionalRecordView(target),
			RollbackImpact:       buildRollbackImpact(deps, c, current, target),
		}
		if c.IsAborted() {
			return
		}
		c.JSON(http.StatusOK, gin.H{"release_state": view})
	}
}

func toOptionalRecordView(record *store.ReleaseRecord) *recordView {
	if record == nil {
		return nil
	}
	view := toRecordView(record)
	return &view
}

// buildRollbackImpact compares the current release against the rollback
// target with the same semantics as release-comparison: scalar version and
// gate facts, the raw rollback point identifier plus the record it resolves
// to within this environment, and change entries keyed by title. A missing
// target leaves the target-side fields empty; an identical target yields an
// empty, consistent impact.
func buildRollbackImpact(deps Dependencies, c *gin.Context, current, target *store.ReleaseRecord) rollbackImpactView {
	impact := rollbackImpactView{
		FromVersion: current.Version,
		Changes:     []releaseChangeDiffView{},
	}
	versionChanged := current.Version != ""
	gateChanged := current.GateStatus != ""
	identifierChanged := current.RollbackPoint != ""
	targetChanged := false
	if target != nil {
		impact.ToVersion = target.Version
		versionChanged = current.Version != target.Version
		gateChanged = current.GateStatus != target.GateStatus
		identifierChanged = current.RollbackPoint != target.RollbackPoint
		impact.Changes, impact.ChangeSummary = diffComparisonChanges(current.Changes, target.Changes)
	} else {
		impact.Changes, impact.ChangeSummary = diffComparisonChanges(current.Changes, nil)
	}
	impact.Version = releaseScalarDiffView{
		Left: current.Version, Right: impact.ToVersion, Changed: versionChanged,
	}
	impact.GateStatus = releaseScalarDiffView{
		Left: current.GateStatus, Right: gateValueOrEmpty(target), Changed: gateChanged,
	}

	var currentInnerTarget, targetInnerTarget *store.ReleaseRecord
	currentInnerTarget = resolveImpactTarget(deps, c, current)
	if c.IsAborted() {
		return impact
	}
	if target != nil {
		targetInnerTarget = resolveImpactTarget(deps, c, target)
		if c.IsAborted() {
			return impact
		}
		targetChanged = !sameReleaseObject(currentInnerTarget, targetInnerTarget)
	}
	var currentInnerView, targetInnerView *recordView
	if currentInnerTarget != nil {
		view := toRecordView(currentInnerTarget)
		currentInnerView = &view
	}
	if targetInnerTarget != nil {
		view := toRecordView(targetInnerTarget)
		targetInnerView = &view
	}
	impact.RollbackPoint = rollbackPointDiffView{
		Left:          current.RollbackPoint,
		Right:         rollbackPointOrEmpty(target),
		Changed:       identifierChanged || (target != nil && targetChanged),
		LeftTarget:    currentInnerView,
		RightTarget:   targetInnerView,
		TargetChanged: targetChanged,
	}
	impact.Consistent = target != nil && !versionChanged && !gateChanged &&
		!identifierChanged && !targetChanged &&
		impact.ChangeSummary.Added == 0 && impact.ChangeSummary.Missing == 0 &&
		impact.ChangeSummary.Changed == 0
	return impact
}

func resolveImpactTarget(deps Dependencies, c *gin.Context, record *store.ReleaseRecord) *store.ReleaseRecord {
	resolved, err := deps.Store.EffectiveReleaseRecord(record.Environment, record.RollbackPoint)
	if err != nil {
		fail(c, http.StatusServiceUnavailable, store.CodeStorageUnavailable, "database is not available")
		return nil
	}
	return resolved
}

func gateValueOrEmpty(record *store.ReleaseRecord) string {
	if record == nil {
		return ""
	}
	return record.GateStatus
}

func rollbackPointOrEmpty(record *store.ReleaseRecord) string {
	if record == nil {
		return ""
	}
	return record.RollbackPoint
}
