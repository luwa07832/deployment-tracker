// Package v1 cross-environment release comparison and per-environment
// release history. Both endpoints are read-only; release facts are written
// solely through POST /api/v1/release-records.
package v1

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/luwa07832/deployment-tracker/internal/store"
)

const (
	defaultHistoryLimit = 20
	maxHistoryLimit     = 100
)

type comparisonSideView struct {
	ID            string `json:"id"`
	Environment   string `json:"environment"`
	Version       string `json:"version"`
	GateStatus    string `json:"gate_status"`
	RollbackPoint string `json:"rollback_point"`
	RecordedAt    string `json:"recorded_at"`
}

func toComparisonSideView(record *store.ReleaseRecord) comparisonSideView {
	return comparisonSideView{
		ID:            record.PublicID,
		Environment:   record.Environment,
		Version:       record.Version,
		GateStatus:    record.GateStatus,
		RollbackPoint: record.RollbackPoint,
		RecordedAt:    record.RecordedAt,
	}
}

type releaseScalarDiffView struct {
	Left    string `json:"left"`
	Right   string `json:"right"`
	Changed bool   `json:"changed"`
}

type rollbackPointDiffView struct {
	Left          string      `json:"left"`
	Right         string      `json:"right"`
	Changed       bool        `json:"changed"`
	LeftTarget    *recordView `json:"left_target"`
	RightTarget   *recordView `json:"right_target"`
	TargetChanged bool        `json:"target_changed"`
}

type changeSummaryView struct {
	Left  string `json:"left,omitempty"`
	Right string `json:"right,omitempty"`
}

type changePresenceView struct {
	Left  bool `json:"left"`
	Right bool `json:"right"`
}

type releaseChangeDiffView struct {
	ChangeID   string             `json:"change_id"`
	Summary    changeSummaryView  `json:"summary"`
	Present    changePresenceView `json:"present"`
	Comparison string             `json:"comparison"`
	Left       *changeEntryView   `json:"left,omitempty"`
	Right      *changeEntryView   `json:"right,omitempty"`
}

type releaseChangeTotalsView struct {
	Added   int `json:"added"`
	Missing int `json:"missing"`
	Changed int `json:"changed"`
}

type releaseComparisonView struct {
	LeftEnvironment  string                  `json:"left_environment"`
	LeftVersion      string                  `json:"left_version"`
	RightEnvironment string                  `json:"right_environment"`
	RightVersion     string                  `json:"right_version"`
	Left             comparisonSideView      `json:"left"`
	Right            comparisonSideView      `json:"right"`
	Version          releaseScalarDiffView   `json:"version"`
	GateStatus       releaseScalarDiffView   `json:"gate_status"`
	RollbackPoint    rollbackPointDiffView   `json:"rollback_point"`
	Changes          []releaseChangeDiffView `json:"changes"`
	ChangeSummary    releaseChangeTotalsView `json:"change_summary"`
	Consistent       bool                    `json:"consistent"`
}

// compareReleases compares one explicitly selected release from each of two
// target environments. Environment names and version identifiers are taken
// verbatim from the query string and matched against the stored values.
func compareReleases(deps Dependencies) gin.HandlerFunc {
	return func(c *gin.Context) {
		left := c.Query("left")
		leftVersion := c.Query("left_version")
		right := c.Query("right")
		rightVersion := c.Query("right_version")
		if strings.TrimSpace(left) == "" || strings.TrimSpace(leftVersion) == "" ||
			strings.TrimSpace(right) == "" || strings.TrimSpace(rightVersion) == "" {
			fail(c, http.StatusBadRequest, store.CodeInvalidReleaseComparisonQuery,
				"left, left_version, right and right_version query parameters are required")
			return
		}
		if !requireComparisonEnvironment(c, deps, left) {
			return
		}
		if !requireComparisonEnvironment(c, deps, right) {
			return
		}
		leftRecord, ok := requireComparisonRelease(c, deps, left, leftVersion)
		if !ok {
			return
		}
		rightRecord, ok := requireComparisonRelease(c, deps, right, rightVersion)
		if !ok {
			return
		}
		leftTarget, rightTarget, ok := requireRollbackTargets(c, deps, leftRecord, rightRecord)
		if !ok {
			return
		}
		c.JSON(http.StatusOK, buildReleaseComparison(leftRecord, rightRecord, leftTarget, rightTarget))
	}
}

// requireComparisonRelease resolves the exact (environment, version) release.
// The version is matched verbatim; unknown versions are a distinct error.
func requireComparisonRelease(c *gin.Context, deps Dependencies, environment, version string) (*store.ReleaseRecord, bool) {
	record, err := deps.Store.EffectiveReleaseRecord(environment, version)
	if err != nil {
		failStorage(c)
		return nil, false
	}
	if record == nil {
		fail(c, http.StatusNotFound, store.CodeReleaseComparisonVersionNotFound,
			"version "+version+" is not recorded for environment "+environment)
		return nil, false
	}
	return record, true
}

// requireRollbackTargets resolves the release object each rollback point
// points at within its own environment. A rollback point whose target release
// is absent means the comparison lacks necessary release data.
func requireRollbackTargets(c *gin.Context, deps Dependencies, left, right *store.ReleaseRecord) (*store.ReleaseRecord, *store.ReleaseRecord, bool) {
	leftTarget, err := deps.Store.EffectiveReleaseRecord(left.Environment, left.RollbackPoint)
	if err != nil {
		failStorage(c)
		return nil, nil, false
	}
	if leftTarget == nil {
		fail(c, http.StatusUnprocessableEntity, store.CodeReleaseComparisonDataIncomplete,
			"rollback point "+left.RollbackPoint+" of "+left.Environment+" "+left.Version+" does not point to a recorded release")
		return nil, nil, false
	}
	rightTarget, err := deps.Store.EffectiveReleaseRecord(right.Environment, right.RollbackPoint)
	if err != nil {
		failStorage(c)
		return nil, nil, false
	}
	if rightTarget == nil {
		fail(c, http.StatusUnprocessableEntity, store.CodeReleaseComparisonDataIncomplete,
			"rollback point "+right.RollbackPoint+" of "+right.Environment+" "+right.Version+" does not point to a recorded release")
		return nil, nil, false
	}
	return leftTarget, rightTarget, true
}

// buildReleaseComparison computes the deterministic, unified comparison
// result. Change entries are keyed by their stable title; only entries that
// differ (added, missing or changed content) appear, sorted by title.
func buildReleaseComparison(left, right, leftTarget, rightTarget *store.ReleaseRecord) releaseComparisonView {
	identifierChanged := left.RollbackPoint != right.RollbackPoint
	targetChanged := !sameReleaseObject(leftTarget, rightTarget)
	changes, totals := diffComparisonChanges(left.Changes, right.Changes)
	versionChanged := left.Version != right.Version
	gateChanged := left.GateStatus != right.GateStatus
	leftTargetView := toRecordView(leftTarget)
	rightTargetView := toRecordView(rightTarget)
	return releaseComparisonView{
		LeftEnvironment:  left.Environment,
		LeftVersion:      left.Version,
		RightEnvironment: right.Environment,
		RightVersion:     right.Version,
		Left:             toComparisonSideView(left),
		Right:            toComparisonSideView(right),
		Version: releaseScalarDiffView{
			Left: left.Version, Right: right.Version, Changed: versionChanged,
		},
		GateStatus: releaseScalarDiffView{
			Left: left.GateStatus, Right: right.GateStatus, Changed: gateChanged,
		},
		RollbackPoint: rollbackPointDiffView{
			Left:          left.RollbackPoint,
			Right:         right.RollbackPoint,
			Changed:       identifierChanged || targetChanged,
			LeftTarget:    &leftTargetView,
			RightTarget:   &rightTargetView,
			TargetChanged: targetChanged,
		},
		Changes:       changes,
		ChangeSummary: totals,
		Consistent: !versionChanged && !gateChanged && !identifierChanged && !targetChanged &&
			totals.Added == 0 && totals.Missing == 0 && totals.Changed == 0,
	}
}

// sameReleaseObject compares the release object a rollback point points to:
// its version, the gate status recorded for it and the full content of its
// change entries, grouped by title and paired by ascending sequence. The
// environment is not part of the comparison because each side lives in its
// own environment.
func sameReleaseObject(a, b *store.ReleaseRecord) bool {
	if a.Version != b.Version || a.GateStatus != b.GateStatus {
		return false
	}
	if len(a.Changes) != len(b.Changes) {
		return false
	}
	for _, pair := range pairChangesByTitle(a.Changes, b.Changes) {
		if pair.Left == nil || pair.Right == nil || !sameEntry(*pair.Left, *pair.Right) {
			return false
		}
	}
	return true
}

// diffComparisonChanges partitions the two entry sets in the left -> right
// direction: added entries exist only on the right, missing only on the
// left, changed entries share a title but differ in sequence, category or
// description. Entries are grouped by title and paired inside each group by
// ascending sequence, so historical records with repeated titles compare
// every entry. The output is sorted by title then sequence.
func diffComparisonChanges(leftEntries, rightEntries []store.ChangeEntry) ([]releaseChangeDiffView, releaseChangeTotalsView) {
	diffs := []releaseChangeDiffView{}
	totals := releaseChangeTotalsView{}
	for _, pair := range pairChangesByTitle(leftEntries, rightEntries) {
		title := pair.Title
		switch {
		case pair.Left == nil:
			rightEntry := *pair.Right
			entry := changeEntryView(rightEntry)
			diffs = append(diffs, releaseChangeDiffView{
				ChangeID:   title,
				Summary:    changeSummaryView{Right: rightEntry.Description},
				Present:    changePresenceView{Left: false, Right: true},
				Comparison: "added",
				Right:      &entry,
			})
			totals.Added++
		case pair.Right == nil:
			leftEntry := *pair.Left
			entry := changeEntryView(leftEntry)
			diffs = append(diffs, releaseChangeDiffView{
				ChangeID:   title,
				Summary:    changeSummaryView{Left: leftEntry.Description},
				Present:    changePresenceView{Left: true, Right: false},
				Comparison: "missing",
				Left:       &entry,
			})
			totals.Missing++
		case !sameEntry(*pair.Left, *pair.Right):
			leftEntry := *pair.Left
			rightEntry := *pair.Right
			leftView := changeEntryView(leftEntry)
			rightView := changeEntryView(rightEntry)
			diffs = append(diffs, releaseChangeDiffView{
				ChangeID:   title,
				Summary:    changeSummaryView{Left: leftEntry.Description, Right: rightEntry.Description},
				Present:    changePresenceView{Left: true, Right: true},
				Comparison: "changed",
				Left:       &leftView,
				Right:      &rightView,
			})
			totals.Changed++
		}
	}
	sortComparisonChanges(diffs)
	return diffs, totals
}

// comparisonChangeOrder is the stable title/sequence key shared by the
// release-comparison and rollback-impact change lists.
func comparisonChangeOrder(diff releaseChangeDiffView) string {
	entry := diff.Right
	if entry == nil {
		entry = diff.Left
	}
	return changeViewOrderKey(*entry)
}

func sortComparisonChanges(diffs []releaseChangeDiffView) {
	sort.Slice(diffs, func(i, j int) bool {
		return comparisonChangeOrder(diffs[i]) < comparisonChangeOrder(diffs[j])
	})
}

// historyCursor is the opaque keyset marker returned to clients. It carries
// the internal position of the last row already returned.
type historyCursor struct {
	RecordedAt string `json:"recorded_at"`
	ID         int64  `json:"id"`
}

func encodeHistoryCursor(cursor historyCursor) string {
	raw, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeHistoryCursor(raw string) (historyCursor, bool) {
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(raw))
	if err != nil {
		return historyCursor{}, false
	}
	var cursor historyCursor
	if err := json.Unmarshal(decoded, &cursor); err != nil || cursor.RecordedAt == "" || cursor.ID <= 0 {
		return historyCursor{}, false
	}
	return cursor, true
}

type releaseHistoryView struct {
	Environment string       `json:"environment"`
	Limit       int          `json:"limit"`
	NextCursor  string       `json:"next_cursor"`
	Releases    []recordView `json:"releases"`
}

// listReleaseHistory returns a stable page of an environment's releases in
// recorded-time order (newest first, insertion order breaking ties).
func listReleaseHistory(deps Dependencies) gin.HandlerFunc {
	return func(c *gin.Context) {
		environment := c.Param("environment")
		limit := defaultHistoryLimit
		if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed < 1 || parsed > maxHistoryLimit {
				fail(c, http.StatusBadRequest, store.CodeInvalidHistoryPagination,
					"limit must be an integer between 1 and "+strconv.Itoa(maxHistoryLimit))
				return
			}
			limit = parsed
		}
		var afterRecordedAt string
		var afterID int64
		if raw := strings.TrimSpace(c.Query("cursor")); raw != "" {
			cursor, ok := decodeHistoryCursor(raw)
			if !ok {
				fail(c, http.StatusBadRequest, store.CodeInvalidHistoryPagination,
					"cursor is not a valid pagination cursor")
				return
			}
			afterRecordedAt = cursor.RecordedAt
			afterID = cursor.ID
		}
		if !requireComparisonEnvironment(c, deps, environment) {
			return
		}
		records, err := deps.Store.ListReleaseRecordHistory(environment, afterRecordedAt, afterID, limit)
		if err != nil {
			failStorage(c)
			return
		}
		nextCursor := ""
		if len(records) > limit {
			last := records[limit-1]
			nextCursor = encodeHistoryCursor(historyCursor{RecordedAt: last.RecordedAt, ID: last.ID})
			records = records[:limit]
		}
		c.JSON(http.StatusOK, releaseHistoryView{
			Environment: environment,
			Limit:       limit,
			NextCursor:  nextCursor,
			Releases:    toRecordViews(records),
		})
	}
}

// requireComparisonEnvironment writes ReleaseComparisonEnvironmentNotFound
// when the environment is not registered, keeping the comparison/history
// endpoints' error semantics distinct from the generic record API.
func requireComparisonEnvironment(c *gin.Context, deps Dependencies, key string) bool {
	exists, err := deps.Store.TrackedEnvironmentExists(key)
	if err != nil {
		failStorage(c)
		return false
	}
	if !exists {
		fail(c, http.StatusNotFound, store.CodeReleaseComparisonEnvironmentNotFound,
			"environment "+key+" is not registered")
		return false
	}
	return true
}
