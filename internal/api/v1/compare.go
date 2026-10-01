package v1

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/luwa07832/deployment-tracker/internal/store"
)

type fieldDiffView struct {
	Field string `json:"field"`
	Left  string `json:"left"`
	Right string `json:"right"`
}

type gateStatusDiffView struct {
	Left    string `json:"left"`
	Right   string `json:"right"`
	Changed bool   `json:"changed"`
}

type changedEntryView struct {
	Left  changeEntryView `json:"left"`
	Right changeEntryView `json:"right"`
}

type versionDiffView struct {
	Version        string             `json:"version"`
	FieldDiffs     []fieldDiffView    `json:"field_diffs"`
	GateStatus     gateStatusDiffView `json:"gate_status"`
	AddedChanges   []changeEntryView  `json:"added_changes"`
	RemovedChanges []changeEntryView  `json:"removed_changes"`
	ChangedChanges []changedEntryView `json:"changed_changes"`
}

type comparisonView struct {
	Left              string            `json:"left"`
	Right             string            `json:"right"`
	BaselineVersion   string            `json:"baseline_version,omitempty"`
	AsOf              string            `json:"as_of,omitempty"`
	ComparedAt        string            `json:"compared_at"`
	LeftVersions      []string          `json:"left_versions"`
	RightVersions     []string          `json:"right_versions"`
	CommonVersions    []string          `json:"common_versions"`
	OnlyLeftVersions  []string          `json:"only_left_versions"`
	OnlyRightVersions []string          `json:"only_right_versions"`
	VersionDiffs      []versionDiffView `json:"version_diffs"`
}

// compareEnvironments diffs two environments over an optional range: an exact
// baseline version ("version") or an inclusive cutoff time ("as_of").
func compareEnvironments(deps Dependencies) gin.HandlerFunc {
	return func(c *gin.Context) {
		left := strings.TrimSpace(c.Query("left"))
		right := strings.TrimSpace(c.Query("right"))
		version := strings.TrimSpace(c.Query("version"))
		rawAsOf := strings.TrimSpace(c.Query("as_of"))
		if left == "" || right == "" {
			fail(c, http.StatusBadRequest, store.CodeInvalidCompareRangeV1,
				"left and right query parameters are required")
			return
		}
		if left == right {
			fail(c, http.StatusBadRequest, store.CodeSameEnvironmentCompareV1,
				"cannot compare an environment with itself")
			return
		}
		if version != "" && rawAsOf != "" {
			fail(c, http.StatusBadRequest, store.CodeInvalidCompareRangeV1,
				"provide either version or as_of, not both")
			return
		}
		asOf := ""
		if rawAsOf != "" {
			bound, err := parseTimeBound(rawAsOf, true)
			if err != nil {
				fail(c, http.StatusBadRequest, store.CodeInvalidCompareRangeV1,
					"as_of must be an RFC3339 timestamp or a YYYY-MM-DD date")
				return
			}
			asOf = bound
		}
		if !requireEnvironment(c, deps, left) || !requireEnvironment(c, deps, right) {
			return
		}
		leftReleases, err := deps.Store.ListReleaseRecords(store.ReleaseRecordFilter{Environment: left})
		if err != nil {
			failStorage(c)
			return
		}
		rightReleases, err := deps.Store.ListReleaseRecords(store.ReleaseRecordFilter{Environment: right})
		if err != nil {
			failStorage(c)
			return
		}
		// An exact named version unknown to both sides is an error, never an
		// empty diff folded away. A time cutoff legitimately contains nothing.
		if version != "" && !versionKnown(version, leftReleases, rightReleases) {
			fail(c, http.StatusBadRequest, store.CodeReleaseVersionNotFoundV1,
				"version "+version+" exists in neither environment")
			return
		}
		view := buildComparison(compareInputs{
			Left:          left,
			Right:         right,
			Version:       version,
			AsOf:          asOf,
			LeftReleases:  leftReleases,
			RightReleases: rightReleases,
			ComparedAt:    time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		})
		c.JSON(http.StatusOK, view)
	}
}

// versionKnown reports whether the exact version string appears on either
// side, ignoring the cutoff so an out-of-range named version still counts as
// a known version.
func versionKnown(version string, sides ...[]store.ReleaseRecord) bool {
	for _, records := range sides {
		for i := range records {
			if records[i].Version == version {
				return true
			}
		}
	}
	return false
}

func failStorage(c *gin.Context) {
	fail(c, http.StatusInternalServerError, store.CodeStorageUnavailable, "database is not available")
}

type compareInputs struct {
	Left          string
	Right         string
	Version       string
	AsOf          string
	LeftReleases  []store.ReleaseRecord
	RightReleases []store.ReleaseRecord
	ComparedAt    string
}

// buildComparison computes the deterministic comparison. With an exact
// version only that version participates; with an as_of cutoff, records
// registered at or before the instant participate; otherwise every traceable
// record does.
func buildComparison(in compareInputs) comparisonView {
	leftEffective := effectiveRecords(in.LeftReleases, in.Version, in.AsOf)
	rightEffective := effectiveRecords(in.RightReleases, in.Version, in.AsOf)
	leftVersions := sortVersions(versionKeys(leftEffective))
	rightVersions := sortVersions(versionKeys(rightEffective))
	common, onlyLeft, onlyRight := partitionVersions(leftVersions, rightVersions)
	diffs := make([]versionDiffView, 0, len(common))
	for _, version := range common {
		diffs = append(diffs, diffVersion(version, leftEffective[version], rightEffective[version]))
	}
	return comparisonView{
		Left:              in.Left,
		Right:             in.Right,
		BaselineVersion:   in.Version,
		AsOf:              in.AsOf,
		ComparedAt:        in.ComparedAt,
		LeftVersions:      leftVersions,
		RightVersions:     rightVersions,
		CommonVersions:    common,
		OnlyLeftVersions:  onlyLeft,
		OnlyRightVersions: onlyRight,
		VersionDiffs:      diffs,
	}
}

// effectiveRecords indexes the records that participate in the comparison by
// version. New records are unique per (environment, version), but the same
// latest-wins read semantics as the baseline are kept.
func effectiveRecords(records []store.ReleaseRecord, version, asOf string) map[string]*store.ReleaseRecord {
	effective := make(map[string]*store.ReleaseRecord)
	for i := range records {
		record := &records[i]
		if version != "" && record.Version != version {
			continue
		}
		if asOf != "" && record.RecordedAt > asOf {
			continue
		}
		effective[record.Version] = record
	}
	return effective
}

// diffVersion describes one shared version in the left -> right direction:
// added change entries exist only on the right, removed only on the left.
func diffVersion(version string, left, right *store.ReleaseRecord) versionDiffView {
	view := versionDiffView{
		Version:        version,
		FieldDiffs:     []fieldDiffView{},
		AddedChanges:   []changeEntryView{},
		RemovedChanges: []changeEntryView{},
		ChangedChanges: []changedEntryView{},
		GateStatus: gateStatusDiffView{
			Left:    left.GateStatus,
			Right:   right.GateStatus,
			Changed: left.GateStatus != right.GateStatus,
		},
	}
	if left.RollbackPoint != right.RollbackPoint {
		view.FieldDiffs = append(view.FieldDiffs, fieldDiffView{
			Field: "rollback_point",
			Left:  left.RollbackPoint,
			Right: right.RollbackPoint,
		})
	}
	leftEntries := indexEntries(left.Changes)
	rightEntries := indexEntries(right.Changes)
	for title, rightEntry := range rightEntries {
		leftEntry, present := leftEntries[title]
		if !present {
			view.AddedChanges = append(view.AddedChanges, toChangeEntryView(rightEntry))
			continue
		}
		if !sameEntry(leftEntry, rightEntry) {
			view.ChangedChanges = append(view.ChangedChanges, changedEntryView{
				Left:  toChangeEntryView(leftEntry),
				Right: toChangeEntryView(rightEntry),
			})
		}
	}
	for title, leftEntry := range leftEntries {
		if _, present := rightEntries[title]; !present {
			view.RemovedChanges = append(view.RemovedChanges, toChangeEntryView(leftEntry))
		}
	}
	sortEntries(view.AddedChanges)
	sortEntries(view.RemovedChanges)
	sort.Slice(view.ChangedChanges, func(i, j int) bool {
		return entryOrder(view.ChangedChanges[i].Left) < entryOrder(view.ChangedChanges[j].Left)
	})
	return view
}

func indexEntries(entries []store.ChangeEntry) map[string]store.ChangeEntry {
	indexed := make(map[string]store.ChangeEntry, len(entries))
	for _, entry := range entries {
		indexed[entry.Title] = entry
	}
	return indexed
}

func toChangeEntryView(entry store.ChangeEntry) changeEntryView {
	return changeEntryView(entry)
}

func sameEntry(a, b store.ChangeEntry) bool {
	return a.Sequence == b.Sequence && a.Category == b.Category && a.Description == b.Description
}

func sortEntries(entries []changeEntryView) {
	sort.Slice(entries, func(i, j int) bool {
		return entryOrder(entries[i]) < entryOrder(entries[j])
	})
}

func entryOrder(entry changeEntryView) string {
	return padSequence(entry.Sequence) + "\x00" + entry.Title
}

// padSequence makes sequence ordering lexical regardless of digit count.
func padSequence(sequence int) string {
	digits := strconv.Itoa(sequence)
	return strings.Repeat("0", 12-len(digits)) + digits
}

func versionKeys(records map[string]*store.ReleaseRecord) []string {
	versions := make([]string, 0, len(records))
	for version := range records {
		versions = append(versions, version)
	}
	return versions
}

// partitionVersions splits two sorted version lists into the intersection and
// the single-side remainders. Outputs keep sorted order and are never nil.
func partitionVersions(left, right []string) (common, onlyLeft, onlyRight []string) {
	common, onlyLeft, onlyRight = []string{}, []string{}, []string{}
	rightSet := make(map[string]bool, len(right))
	for _, version := range right {
		rightSet[version] = true
	}
	leftSet := make(map[string]bool, len(left))
	for _, version := range left {
		leftSet[version] = true
		if rightSet[version] {
			common = append(common, version)
		} else {
			onlyLeft = append(onlyLeft, version)
		}
	}
	for _, version := range right {
		if !leftSet[version] {
			onlyRight = append(onlyRight, version)
		}
	}
	return common, onlyLeft, onlyRight
}
