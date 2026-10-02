// release_state.go serves the per-environment release-state and rollback
// impact query. It is strictly read-only: release facts are written solely
// through POST /api/v1/release-records.
package v1

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/luwa07832/deployment-tracker/internal/store"
)

type releaseStateView struct {
	Environment          string              `json:"environment"`
	AsOf                 string              `json:"as_of"`
	CurrentRelease       *recordView         `json:"current_release"`
	PreviousRelease      *recordView         `json:"previous_release"`
	RollbackTargetStatus string              `json:"rollback_target_status"`
	RollbackTarget       *recordView         `json:"rollback_target"`
	RollbackImpact       *rollbackImpactView `json:"rollback_impact"`
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

// getReleaseState returns the release effective in an environment at a point
// in time together with the immediately preceding release and the impact of
// rolling back from the current release to the resolved rollback target.
func getReleaseState(deps Dependencies) gin.HandlerFunc {
	return func(c *gin.Context) {
		environment := c.Param("environment")

		_, hasAsOf := c.GetQuery("as_of")
		rawAsOf := strings.TrimSpace(c.Query("as_of"))
		rawTarget, hasTarget := c.GetQuery("target_version")
		if hasTarget && strings.TrimSpace(rawTarget) == "" {
			fail(c, http.StatusBadRequest, store.CodeInvalidReleaseStateTarget,
				"target_version must not be blank")
			return
		}

		asOf := time.Now().UTC().Format("2006-01-02T15:04:05Z")
		if hasAsOf {
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

		// current/previous selection happens entirely within the cutoff: only
		// records existing at or before as_of participate, newest first with
		// same-timestamp writes keeping insertion order. Two rows are enough.
		latest, err := deps.Store.ListLatestReleaseRecords(environment, asOf, 2)
		if err != nil {
			fail(c, http.StatusServiceUnavailable, store.CodeStorageUnavailable, "database is not available")
			return
		}
		if len(latest) == 0 {
			fail(c, http.StatusNotFound, store.CodeReleaseStateNotFound,
				"no release recorded for environment "+environment+" at or before "+asOf)
			return
		}
		current := &latest[0]
		var previous *store.ReleaseRecord
		if len(latest) > 1 {
			previous = &latest[1]
		}

		target, targetErr := resolveStateRollbackTarget(deps, current, rawTarget, hasTarget, asOf)
		if targetErr != nil {
			writeStateTargetError(c, rawTarget, targetErr)
			return
		}
		status := "missing"
		if target != nil {
			status = "resolved"
		}
		if target != nil && hasTarget {
			status = "requested"
		}

		// The impact compares rollback-point target objects too. Resolution is
		// best effort within the cutoff so an unregistered rollback point
		// never turns a readable release state into an error.
		currentPointTarget, err := deps.Store.EffectiveReleaseRecordAsOf(
			current.Environment, current.RollbackPoint, asOf)
		if err != nil {
			fail(c, http.StatusServiceUnavailable, store.CodeStorageUnavailable, "database is not available")
			return
		}
		var targetPointTarget *store.ReleaseRecord
		if target != nil {
			targetPointTarget, err = deps.Store.EffectiveReleaseRecordAsOf(
				target.Environment, target.RollbackPoint, asOf)
			if err != nil {
				fail(c, http.StatusServiceUnavailable, store.CodeStorageUnavailable, "database is not available")
				return
			}
		}

		currentView := toRecordView(current)
		state := releaseStateView{
			Environment:          environment,
			AsOf:                 asOf,
			CurrentRelease:       &currentView,
			RollbackTargetStatus: status,
			RollbackImpact: buildRollbackImpact(
				current, target, currentPointTarget, targetPointTarget),
		}
		if previous != nil {
			previousView := toRecordView(previous)
			state.PreviousRelease = &previousView
		}
		if target != nil {
			targetView := toRecordView(target)
			state.RollbackTarget = &targetView
		}
		c.JSON(http.StatusOK, state)
	}
}

// stateTargetError distinguishes "target release absent" (a client-facing
// 404 only for explicitly requested versions) from storage failures.
type stateTargetError struct {
	notFound bool
}

func writeStateTargetError(c *gin.Context, rawTarget string, targetErr *stateTargetError) {
	if targetErr.notFound {
		fail(c, http.StatusNotFound, store.CodeRollbackTargetNotFound,
			"version "+rawTarget+" is not recorded for this environment")
		return
	}
	fail(c, http.StatusServiceUnavailable, store.CodeStorageUnavailable, "database is not available")
}

// resolveStateRollbackTarget resolves the release a rollback would land on.
// Without target_version the current record's rollback_point value is used
// verbatim and must have been recorded at or before as_of (an absent target
// is a regular "missing" outcome). With target_version the verbatim version
// identifier is resolved regardless of the cutoff, and an absent target is a
// hard ROLLBACK_TARGET_NOT_FOUND.
func resolveStateRollbackTarget(
	deps Dependencies,
	current *store.ReleaseRecord,
	targetVersion string,
	hasTarget bool,
	asOf string,
) (*store.ReleaseRecord, *stateTargetError) {
	if hasTarget {
		target, err := deps.Store.EffectiveReleaseRecord(current.Environment, targetVersion)
		if err != nil {
			return nil, &stateTargetError{}
		}
		if target == nil {
			return nil, &stateTargetError{notFound: true}
		}
		return target, nil
	}
	target, err := deps.Store.EffectiveReleaseRecordAsOf(current.Environment, current.RollbackPoint, asOf)
	if err != nil {
		return nil, &stateTargetError{}
	}
	return target, nil
}

// buildRollbackImpact compares the current release against the rollback
// target using release-comparison semantics: version, gate status, the
// rollback point identifier plus its resolved target object, and change
// entries keyed by title. Direction is current -> target. A nil target means
// the impact describes every current fact as lost; a target identical to the
// current record yields an empty, consistent diff.
//
// pointTarget arguments are the rollback-point identifiers of current and
// target respectively, resolved within the cutoff; nil means unresolved.
func buildRollbackImpact(
	current, target, currentPointTarget, targetPointTarget *store.ReleaseRecord,
) *rollbackImpactView {
	if target == nil {
		return buildMissingRollbackImpact(current, currentPointTarget)
	}
	changes, totals := diffComparisonChanges(current.Changes, target.Changes)
	versionChanged := current.Version != target.Version
	gateChanged := current.GateStatus != target.GateStatus
	identifierChanged := current.RollbackPoint != target.RollbackPoint
	pointTargetChanged := !resolvedObjectsEqual(currentPointTarget, targetPointTarget)
	currentPointView := recordPointerView(currentPointTarget)
	targetPointView := recordPointerView(targetPointTarget)
	return &rollbackImpactView{
		FromVersion: current.Version,
		ToVersion:   target.Version,
		Version: releaseScalarDiffView{
			Left: current.Version, Right: target.Version, Changed: versionChanged,
		},
		GateStatus: releaseScalarDiffView{
			Left: current.GateStatus, Right: target.GateStatus, Changed: gateChanged,
		},
		RollbackPoint: rollbackPointDiffView{
			Left:          current.RollbackPoint,
			Right:         target.RollbackPoint,
			Changed:       identifierChanged || pointTargetChanged,
			LeftTarget:    currentPointView,
			RightTarget:   targetPointView,
			TargetChanged: pointTargetChanged,
		},
		Changes:       changes,
		ChangeSummary: totals,
		Consistent: !versionChanged && !gateChanged && !identifierChanged && !pointTargetChanged &&
			totals.Added == 0 && totals.Missing == 0 && totals.Changed == 0,
	}
}

// buildMissingRollbackImpact renders the impact when no rollback target
// release can be resolved: scalar facts and changes come from the current
// release only, and the rollback point's target object stays absent.
func buildMissingRollbackImpact(current, currentPointTarget *store.ReleaseRecord) *rollbackImpactView {
	changes, totals := diffComparisonChanges(current.Changes, nil)
	return &rollbackImpactView{
		FromVersion: current.Version,
		Version:     releaseScalarDiffView{Left: current.Version, Changed: true},
		GateStatus:  releaseScalarDiffView{Left: current.GateStatus, Changed: true},
		RollbackPoint: rollbackPointDiffView{
			Left:       current.RollbackPoint,
			Changed:    true,
			LeftTarget: recordPointerView(currentPointTarget),
		},
		Changes:       changes,
		ChangeSummary: totals,
		Consistent:    false,
	}
}

// resolvedObjectsEqual compares two best-effort resolved rollback-point
// target objects. Two absent objects cannot be told apart and count as equal;
// presence on only one side is a difference.
func resolvedObjectsEqual(a, b *store.ReleaseRecord) bool {
	if a == nil || b == nil {
		return a == b
	}
	return sameReleaseObject(a, b)
}

func recordPointerView(record *store.ReleaseRecord) *recordView {
	if record == nil {
		return nil
	}
	view := toRecordView(record)
	return &view
}
