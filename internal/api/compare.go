package api

import (
	"net/http"
	"sort"

	"github.com/gin-gonic/gin"

	"github.com/luwa07832/deployment-tracker/internal/store"
)

// versionDiffResponse describes how one shared version differs between the
// two compared environments. Added and removed are read in the direction
// left -> right: added entries exist only on the right, removed only on the
// left.
type versionDiffResponse struct {
	Version           string   `json:"version"`
	AddedChanges      []string `json:"added_changes"`
	RemovedChanges    []string `json:"removed_changes"`
	LeftGateStatus    *string  `json:"left_gate_status"`
	RightGateStatus   *string  `json:"right_gate_status"`
	GateStatusChanged bool     `json:"gate_status_changed"`
}

// comparisonResponse is the deterministic result of comparing two
// environments. Every list is always present, empty lists stay empty arrays.
type comparisonResponse struct {
	Left                string                `json:"left"`
	Right               string                `json:"right"`
	Until               string                `json:"until,omitempty"`
	LeftVersions        []string              `json:"left_versions"`
	RightVersions       []string              `json:"right_versions"`
	CommonVersions      []string              `json:"common_versions"`
	OnlyLeftVersions    []string              `json:"only_left_versions"`
	OnlyRightVersions   []string              `json:"only_right_versions"`
	VersionDiffs        []versionDiffResponse `json:"version_diffs"`
	LeftRollbackPoints  []string              `json:"left_rollback_points"`
	RightRollbackPoints []string              `json:"right_rollback_points"`
}

// compareEnvironments diffs the traceable records of two environments, each
// limited to versions at or below the optional "until" cutoff.
func compareEnvironments(deps Dependencies) gin.HandlerFunc {
	return func(c *gin.Context) {
		left := c.Query("left")
		right := c.Query("right")
		until := c.Query("until")
		if left == "" || right == "" {
			fail(c, http.StatusBadRequest, store.CodeInvalidRequest, "left and right query parameters are required")
			return
		}
		if left == right {
			fail(c, http.StatusConflict, store.CodeComparisonConflict, "cannot compare an environment with itself")
			return
		}
		if !environmentExists(c, deps, left) || !environmentExists(c, deps, right) {
			return
		}
		leftReleases, err := deps.Store.ListReleases(store.ReleaseFilter{Environment: left})
		if err != nil {
			fail(c, http.StatusInternalServerError, store.CodeStorageUnavailable, "database is not available")
			return
		}
		rightReleases, err := deps.Store.ListReleases(store.ReleaseFilter{Environment: right})
		if err != nil {
			fail(c, http.StatusInternalServerError, store.CodeStorageUnavailable, "database is not available")
			return
		}
		c.JSON(http.StatusOK, buildComparison(left, right, until, leftReleases, rightReleases))
	}
}

// buildComparison computes the diff. Releases must arrive in registration
// order; for each version the record registered last is the effective one.
func buildComparison(left, right, until string, leftReleases, rightReleases []store.Release) comparisonResponse {
	leftEffective := effectiveReleases(leftReleases, until)
	rightEffective := effectiveReleases(rightReleases, until)
	leftVersions := sortVersions(versionSet(leftEffective))
	rightVersions := sortVersions(versionSet(rightEffective))
	common, onlyLeft, onlyRight := partitionVersions(leftVersions, rightVersions)
	diffs := make([]versionDiffResponse, 0, len(common))
	for _, version := range common {
		leftRelease := leftEffective[version]
		rightRelease := rightEffective[version]
		diffs = append(diffs, versionDiffResponse{
			Version:           version,
			AddedChanges:      stringSetDiff(rightRelease.Changes, leftRelease.Changes),
			RemovedChanges:    stringSetDiff(leftRelease.Changes, rightRelease.Changes),
			LeftGateStatus:    leftRelease.GateStatus,
			RightGateStatus:   rightRelease.GateStatus,
			GateStatusChanged: !equalStringPtr(leftRelease.GateStatus, rightRelease.GateStatus),
		})
	}
	return comparisonResponse{
		Left:                left,
		Right:               right,
		Until:               until,
		LeftVersions:        leftVersions,
		RightVersions:       rightVersions,
		CommonVersions:      common,
		OnlyLeftVersions:    onlyLeft,
		OnlyRightVersions:   onlyRight,
		VersionDiffs:        diffs,
		LeftRollbackPoints:  rollbackPoints(leftEffective, leftVersions),
		RightRollbackPoints: rollbackPoints(rightEffective, rightVersions),
	}
}

// effectiveReleases keeps the latest registered record per version, limited to
// versions at or below the cutoff. An empty cutoff disables the limit.
func effectiveReleases(releases []store.Release, until string) map[string]*store.Release {
	effective := make(map[string]*store.Release)
	for i := range releases {
		rel := &releases[i]
		if until != "" && compareVersions(rel.Version, until) > 0 {
			continue
		}
		effective[rel.Version] = rel
	}
	return effective
}

// versionSet returns the keys of an effective-release map as a slice.
func versionSet(effective map[string]*store.Release) []string {
	versions := make([]string, 0, len(effective))
	for version := range effective {
		versions = append(versions, version)
	}
	return versions
}

// partitionVersions splits two sorted version lists into the intersection and
// the single-side remainders. Inputs stay untouched; outputs keep the sort
// order and are never nil.
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

// rollbackPoints collects the distinct rollback points of the effective
// records, sorted as versions.
func rollbackPoints(effective map[string]*store.Release, versions []string) []string {
	seen := make(map[string]bool)
	points := []string{}
	for _, version := range versions {
		point := effective[version].RollbackPoint
		if point == nil || seen[*point] {
			continue
		}
		seen[*point] = true
		points = append(points, *point)
	}
	return sortVersions(points)
}

// stringSetDiff returns the distinct entries present in from and absent in
// minus, sorted lexically. The result is never nil.
func stringSetDiff(from, minus []string) []string {
	excluded := make(map[string]bool, len(minus))
	for _, entry := range minus {
		excluded[entry] = true
	}
	seen := make(map[string]bool)
	out := []string{}
	for _, entry := range from {
		if excluded[entry] || seen[entry] {
			continue
		}
		seen[entry] = true
		out = append(out, entry)
	}
	sort.Strings(out)
	return out
}
