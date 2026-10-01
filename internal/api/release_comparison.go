package api

import (
	"net/http"
	"sort"

	"github.com/gin-gonic/gin"

	"github.com/luwa07832/deployment-tracker/internal/store"
)

// comparedReleaseView is the immutable release fact embedded in comparison
// and rollback-point results. Every field is present by construction:
// records lacking comparable data are rejected before a view is built.
type comparedReleaseView struct {
	ID            int64    `json:"id"`
	Environment   string   `json:"environment"`
	Version       string   `json:"version"`
	Changes       []string `json:"changes"`
	GateStatus    string   `json:"gate_status"`
	RollbackPoint string   `json:"rollback_point"`
	RegisteredAt  string   `json:"registered_at"`
}

func toComparedReleaseView(rel *store.Release) comparedReleaseView {
	changes := []string{}
	if rel.Changes != nil {
		changes = append(changes, rel.Changes...)
	}
	gateStatus := ""
	if rel.GateStatus != nil {
		gateStatus = *rel.GateStatus
	}
	rollbackPoint := ""
	if rel.RollbackPoint != nil {
		rollbackPoint = *rel.RollbackPoint
	}
	return comparedReleaseView{
		ID:            rel.ID,
		Environment:   rel.Environment,
		Version:       rel.Version,
		Changes:       changes,
		GateStatus:    gateStatus,
		RollbackPoint: rollbackPoint,
		RegisteredAt:  rel.CreatedAt,
	}
}

// scalarComparisonView reports one scalar fact on both sides and whether the
// recorded values agree. Gate status values are the ones stored at release
// time; the service never re-derives them from current state.
type scalarComparisonView struct {
	Left       string `json:"left"`
	Right      string `json:"right"`
	Consistent bool   `json:"consistent"`
}

// rollbackPointSideView names one side's rollback point together with the
// release object it points at, so the comparison covers both the stable
// identifier and its target rather than display text alone.
type rollbackPointSideView struct {
	Identifier    string              `json:"identifier"`
	TargetRelease comparedReleaseView `json:"target_release"`
}

type rollbackPointComparisonView struct {
	Left       rollbackPointSideView `json:"left"`
	Right      rollbackPointSideView `json:"right"`
	Consistent bool                  `json:"consistent"`
}

// releaseChangeDiffView is one change entry compared in the left -> right
// direction. Change identifiers are the recorded entry values verbatim; no
// fuzzy matching is applied. Items always carry the stable identifier,
// summary, per-side presence and a content comparison status.
type releaseChangeDiffView struct {
	ChangeID       string `json:"change_id"`
	Summary        string `json:"summary"`
	Kind           string `json:"kind"`
	PresentOnLeft  bool   `json:"present_on_left"`
	PresentOnRight bool   `json:"present_on_right"`
	ContentStatus  string `json:"content_status"`
	Left           string `json:"left,omitempty"`
	Right          string `json:"right,omitempty"`
}

// releaseComparisonView is the deterministic result of pinning one version on
// each of two environments. It contains no server-generated timestamp, so the
// same inputs produce byte-identical responses.
type releaseComparisonView struct {
	LeftEnvironment  string                      `json:"left_environment"`
	LeftVersion      string                      `json:"left_version"`
	RightEnvironment string                      `json:"right_environment"`
	RightVersion     string                      `json:"right_version"`
	LeftRelease      comparedReleaseView         `json:"left_release"`
	RightRelease     comparedReleaseView         `json:"right_release"`
	Version          scalarComparisonView        `json:"version"`
	GateStatus       scalarComparisonView        `json:"gate_status"`
	RollbackPoint    rollbackPointComparisonView `json:"rollback_point"`
	ChangeDiffs      []releaseChangeDiffView     `json:"change_diffs"`
	Consistent       bool                        `json:"consistent"`
}

// compareReleases answers GET /release-comparisons: the caller pins one
// environment/version pair per side and receives a unified diff instead of
// assembling release data themselves.
func compareReleases(deps Dependencies) gin.HandlerFunc {
	return func(c *gin.Context) {
		left := c.Query("left")
		leftVersion := c.Query("left_version")
		right := c.Query("right")
		rightVersion := c.Query("right_version")
		if left == "" || leftVersion == "" || right == "" || rightVersion == "" {
			fail(c, http.StatusBadRequest, store.CodeInvalidRequest,
				"left, left_version, right and right_version query parameters are required")
			return
		}
		// Environments are validated in fixed side order, then versions in
		// the same order, then data completeness. Every failure therefore
		// maps to exactly one documented error code.
		if !comparisonEnvironmentExists(c, deps, left) {
			return
		}
		if !comparisonEnvironmentExists(c, deps, right) {
			return
		}
		leftRelease, ok := comparisonVersionExists(c, deps, left, leftVersion)
		if !ok {
			return
		}
		rightRelease, ok := comparisonVersionExists(c, deps, right, rightVersion)
		if !ok {
			return
		}
		leftTarget, ok := requireComparableRelease(c, deps, leftRelease, left)
		if !ok {
			return
		}
		rightTarget, ok := requireComparableRelease(c, deps, rightRelease, right)
		if !ok {
			return
		}
		c.JSON(http.StatusOK, buildReleaseComparison(
			left, leftVersion, right, rightVersion,
			leftRelease, rightRelease, leftTarget, rightTarget,
		))
	}
}

// comparisonEnvironmentExists emits ReleaseComparisonEnvironmentNotFound for
// unknown environments. Environment and version identifiers are matched
// verbatim against stored release data, never normalized or fuzzed.
func comparisonEnvironmentExists(c *gin.Context, deps Dependencies, environment string) bool {
	exists, err := deps.Store.EnvironmentExists(environment)
	if err != nil {
		fail(c, http.StatusInternalServerError, store.CodeStorageUnavailable, "database is not available")
		return false
	}
	if !exists {
		fail(c, http.StatusNotFound, store.CodeReleaseComparisonEnvironmentNotFound,
			"no releases recorded for environment "+environment)
		return false
	}
	return true
}

// comparisonVersionExists loads the effective record for one pinned
// environment/version pair and emits ReleaseComparisonVersionNotFound when
// the version was never released there.
func comparisonVersionExists(c *gin.Context, deps Dependencies, environment, version string) (*store.Release, bool) {
	release, err := deps.Store.FindRelease(environment, version)
	if err != nil {
		fail(c, http.StatusInternalServerError, store.CodeStorageUnavailable, "database is not available")
		return nil, false
	}
	if release == nil {
		fail(c, http.StatusNotFound, store.CodeReleaseComparisonVersionNotFound,
			"no release recorded for "+environment+" "+version)
		return nil, false
	}
	return release, true
}

// requireComparableRelease verifies that a pinned release carries the data a
// comparison needs (changes, gate status and rollback point), resolves the
// release object its rollback point identifies, and verifies that target is
// comparable too. A rollback point names the version of the release it rolls
// back to within the same environment, matching the history-traceback
// semantics of GET /environments/{environment}/history.
func requireComparableRelease(c *gin.Context, deps Dependencies, release *store.Release, environment string) (*store.Release, bool) {
	if release.Changes == nil || release.GateStatus == nil || release.RollbackPoint == nil {
		fail(c, http.StatusUnprocessableEntity, store.CodeReleaseComparisonDataIncomplete,
			"release "+environment+" "+release.Version+" is missing changes, gate status or rollback point data")
		return nil, false
	}
	target, err := deps.Store.FindRelease(environment, *release.RollbackPoint)
	if err != nil {
		fail(c, http.StatusInternalServerError, store.CodeStorageUnavailable, "database is not available")
		return nil, false
	}
	if target == nil {
		fail(c, http.StatusUnprocessableEntity, store.CodeReleaseComparisonDataIncomplete,
			"rollback point "+*release.RollbackPoint+" of release "+environment+" "+release.Version+
				" does not identify a recorded release")
		return nil, false
	}
	if target.Changes == nil || target.GateStatus == nil || target.RollbackPoint == nil {
		fail(c, http.StatusUnprocessableEntity, store.CodeReleaseComparisonDataIncomplete,
			"release "+environment+" "+*release.RollbackPoint+" targeted by a rollback point is missing comparison data")
		return nil, false
	}
	return target, true
}

// buildReleaseComparison assembles the stable response. Change entries are
// sets, matching the immutability semantics of POST /releases.
func buildReleaseComparison(leftEnv, leftVersion, rightEnv, rightVersion string,
	leftRelease, rightRelease, leftTarget, rightTarget *store.Release) releaseComparisonView {
	versionComparison := scalarComparisonView{
		Left:       leftRelease.Version,
		Right:      rightRelease.Version,
		Consistent: leftRelease.Version == rightRelease.Version,
	}
	gateComparison := scalarComparisonView{
		Left:       *leftRelease.GateStatus,
		Right:      *rightRelease.GateStatus,
		Consistent: *leftRelease.GateStatus == *rightRelease.GateStatus,
	}
	rollbackComparison := rollbackPointComparisonView{
		Left: rollbackPointSideView{
			Identifier:    *leftRelease.RollbackPoint,
			TargetRelease: toComparedReleaseView(leftTarget),
		},
		Right: rollbackPointSideView{
			Identifier:    *rightRelease.RollbackPoint,
			TargetRelease: toComparedReleaseView(rightTarget),
		},
	}
	rollbackComparison.Consistent = sameRollbackTarget(*leftRelease.RollbackPoint, leftTarget,
		*rightRelease.RollbackPoint, rightTarget)
	changeDiffs := diffReleaseChanges(leftRelease.Changes, rightRelease.Changes)
	view := releaseComparisonView{
		LeftEnvironment:  leftEnv,
		LeftVersion:      leftVersion,
		RightEnvironment: rightEnv,
		RightVersion:     rightVersion,
		LeftRelease:      toComparedReleaseView(leftRelease),
		RightRelease:     toComparedReleaseView(rightRelease),
		Version:          versionComparison,
		GateStatus:       gateComparison,
		RollbackPoint:    rollbackComparison,
		ChangeDiffs:      changeDiffs,
	}
	view.Consistent = versionComparison.Consistent && gateComparison.Consistent &&
		rollbackComparison.Consistent && len(changeDiffs) == 0
	return view
}

// sameRollbackTarget compares both the rollback point identifiers and the
// release objects they point at. Target identity covers the recorded gate
// status, the change set and the target's own rollback point identifier
// (compared one level, without recursion); the target version is the
// identifier itself.
func sameRollbackTarget(leftIdentifier string, leftTarget *store.Release,
	rightIdentifier string, rightTarget *store.Release) bool {
	if leftIdentifier != rightIdentifier {
		return false
	}
	if *leftTarget.GateStatus != *rightTarget.GateStatus {
		return false
	}
	if *leftTarget.RollbackPoint != *rightTarget.RollbackPoint {
		return false
	}
	return stringSetEqual(leftTarget.Changes, rightTarget.Changes)
}

// diffReleaseChanges classifies the union of both change sets in the
// left -> right direction. The union is ordered lexically by the stable
// identifier, which keeps the response repeatable. Entries present on both
// sides with identical content are omitted; an empty diff means the change
// lists fully agree.
func diffReleaseChanges(leftChanges, rightChanges []string) []releaseChangeDiffView {
	leftSet := make(map[string]bool, len(leftChanges))
	for _, entry := range leftChanges {
		leftSet[entry] = true
	}
	rightSet := make(map[string]bool, len(rightChanges))
	for _, entry := range rightChanges {
		rightSet[entry] = true
	}
	identifiers := make([]string, 0, len(leftSet)+len(rightSet))
	seen := map[string]bool{}
	for _, entry := range append(append([]string{}, leftChanges...), rightChanges...) {
		if !seen[entry] {
			seen[entry] = true
			identifiers = append(identifiers, entry)
		}
	}
	sort.Strings(identifiers)
	diffs := []releaseChangeDiffView{}
	for _, identifier := range identifiers {
		onLeft := leftSet[identifier]
		onRight := rightSet[identifier]
		switch {
		case onLeft && onRight:
			// Baseline change entries are atomic strings, so entries sharing
			// the identifier always carry identical content and are omitted.
			continue
		case onRight:
			diffs = append(diffs, releaseChangeDiffView{
				ChangeID:       identifier,
				Summary:        identifier,
				Kind:           "added",
				PresentOnLeft:  false,
				PresentOnRight: true,
				ContentStatus:  "absent",
				Right:          identifier,
			})
		default:
			diffs = append(diffs, releaseChangeDiffView{
				ChangeID:       identifier,
				Summary:        identifier,
				Kind:           "missing",
				PresentOnLeft:  true,
				PresentOnRight: false,
				ContentStatus:  "absent",
				Left:           identifier,
			})
		}
	}
	return diffs
}
