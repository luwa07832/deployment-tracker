package v1

import (
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
)

func queryComparison(t *testing.T, router *gin.Engine, left, leftVersion, right, rightVersion string) map[string]any {
	t.Helper()
	target := "/api/v1/release-comparison?left=" + left + "&left_version=" + leftVersion +
		"&right=" + right + "&right_version=" + rightVersion
	recorder := doRequest(t, router, http.MethodGet, target, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("comparison status = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	return decodeBody(t, recorder)
}

// release-comparison totals and consistency follow the per-entry pairing:
// a repeated title with one extra entry on the right is exactly one "added".
func TestReleaseComparisonRepeatedTitlesCountPerEntry(t *testing.T) {
	router, deps := newTestRouterWithDeps(t)
	legacyEnv(t, deps, "dev")
	legacyEnv(t, deps, "prod")
	// Rollback points resolve inside each environment to records with
	// identical full content, so only the change lists drive consistency.
	legacyDuplicateRecord(t, deps, "dev", "0.9.0", "allowed", "0.8.0",
		dupEntry(1, "feature", "base", "d"))
	legacyDuplicateRecord(t, deps, "prod", "0.9.0", "allowed", "0.8.0",
		dupEntry(1, "feature", "base", "d"))
	legacyDuplicateRecord(t, deps, "dev", "0.8.0", "allowed", "0.8.0",
		dupEntry(1, "feature", "base", "d"))
	legacyDuplicateRecord(t, deps, "prod", "0.8.0", "allowed", "0.8.0",
		dupEntry(1, "feature", "base", "d"))
	legacyDuplicateRecord(t, deps, "dev", "1.0.0", "allowed", "0.9.0",
		dupEntry(1, "feature", "x", "same"),
		dupEntry(2, "feature", "x", "dev only"))
	legacyDuplicateRecord(t, deps, "prod", "1.0.0", "allowed", "0.9.0",
		dupEntry(1, "feature", "x", "same"))

	body := queryComparison(t, router, "dev", "1.0.0", "prod", "1.0.0")
	summary := body["change_summary"].(map[string]any)
	if summary["added"] != float64(0) || summary["missing"] != float64(1) || summary["changed"] != float64(0) {
		t.Fatalf("change_summary = %v, want missing=1", summary)
	}
	if body["consistent"] != false {
		t.Fatalf("consistent = %v, want false with one missing repeated entry", body["consistent"])
	}
	changes := body["changes"].([]any)
	if len(changes) != 1 || changes[0].(map[string]any)["comparison"] != "missing" ||
		changes[0].(map[string]any)["change_id"] != "x" {
		t.Fatalf("changes = %v, want one missing x", changes)
	}
}

// Identical repeated-title sets must compare equal even though the old
// title-index map kept only the last entry's content.
func TestReleaseComparisonIdenticalRepeatedTitlesConsistent(t *testing.T) {
	router, deps := newTestRouterWithDeps(t)
	legacyEnv(t, deps, "a")
	legacyEnv(t, deps, "b")
	legacyDuplicateRecord(t, deps, "a", "0.9.0", "allowed", "0.9.0",
		dupEntry(1, "feature", "x", "one"),
		dupEntry(2, "feature", "x", "two"))
	legacyDuplicateRecord(t, deps, "b", "0.9.0", "allowed", "0.9.0",
		dupEntry(1, "feature", "x", "one"),
		dupEntry(2, "feature", "x", "two"))
	legacyDuplicateRecord(t, deps, "a", "1.0.0", "allowed", "0.9.0",
		dupEntry(1, "feature", "x", "one"),
		dupEntry(2, "feature", "x", "two"))
	legacyDuplicateRecord(t, deps, "b", "1.0.0", "allowed", "0.9.0",
		dupEntry(1, "feature", "x", "one"),
		dupEntry(2, "feature", "x", "two"))

	body := queryComparison(t, router, "a", "1.0.0", "b", "1.0.0")
	if body["consistent"] != true {
		t.Fatalf("consistent = %v, want true (body %v)", body["consistent"], body)
	}
	if changes := body["changes"].([]any); len(changes) != 0 {
		t.Fatalf("changes = %v, want empty", changes)
	}
	summary := body["change_summary"].(map[string]any)
	if summary["added"] != float64(0) || summary["missing"] != float64(0) || summary["changed"] != float64(0) {
		t.Fatalf("change_summary = %v, want all zero", summary)
	}
}

// The rollback-point target objects compare with the full multiset of
// entries: equal repeated sets keep target_changed false even when the last
// stored entry alone would look different from a set without repetition.
func TestReleaseComparisonRollbackTargetsUseFullEntries(t *testing.T) {
	router, deps := newTestRouterWithDeps(t)
	legacyEnv(t, deps, "a")
	legacyEnv(t, deps, "b")
	// Both rollback targets are version 0.9.0 with the same repeated set.
	legacyDuplicateRecord(t, deps, "a", "0.9.0", "allowed", "0.9.0",
		dupEntry(1, "feature", "x", "first"),
		dupEntry(2, "feature", "x", "second"))
	legacyDuplicateRecord(t, deps, "b", "0.9.0", "allowed", "0.9.0",
		dupEntry(1, "feature", "x", "first"),
		dupEntry(2, "feature", "x", "second"))
	// Current records agree on everything, including the same rollback id.
	legacyDuplicateRecord(t, deps, "a", "1.0.0", "allowed", "0.9.0",
		dupEntry(1, "feature", "x", "first"),
		dupEntry(2, "feature", "x", "second"))
	legacyDuplicateRecord(t, deps, "b", "1.0.0", "allowed", "0.9.0",
		dupEntry(1, "feature", "x", "first"),
		dupEntry(2, "feature", "x", "second"))

	body := queryComparison(t, router, "a", "1.0.0", "b", "1.0.0")
	rp := body["rollback_point"].(map[string]any)
	if rp["target_changed"] != false || rp["changed"] != false {
		t.Fatalf("rollback_point = %v, want equal targets", rp)
	}
	if body["consistent"] != true {
		t.Fatalf("consistent = %v, want true", body["consistent"])
	}
}
