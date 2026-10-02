package v1

import (
	"net/http"
	"strings"
	"testing"
)

func entryViewsTitled(changes []any) []string {
	titles := make([]string, 0, len(changes))
	for _, raw := range changes {
		titles = append(titles, raw.(map[string]any)["title"].(string))
	}
	return titles
}

// GET /api/v1/compare must pair same-titled entries positionally by sequence
// and count every surplus repeated entry instead of keeping only one.
func TestCompareRepeatedTitlesPairBySequence(t *testing.T) {
	router, deps := newTestRouterWithDeps(t)
	legacyEnv(t, deps, "left")
	legacyEnv(t, deps, "right")
	// Left: title "x" twice (sequences 1,2) plus "only-left".
	// Right: title "x" twice (sequences 1,3) plus "only-right".
	legacyDuplicateRecord(t, deps, "left", "1.0.0", "allowed", "0.9.0",
		dupEntry(1, "feature", "x", "same"),
		dupEntry(2, "fix", "x", "left second"),
		dupEntry(3, "feature", "only-left", "d"))
	legacyDuplicateRecord(t, deps, "right", "1.0.0", "allowed", "0.9.0",
		dupEntry(1, "feature", "x", "same"),
		dupEntry(3, "fix", "x", "right second"),
		dupEntry(4, "feature", "only-right", "d"))

	body := decodeBody(t, doRequest(t, router, http.MethodGet,
		"/api/v1/compare?left=left&right=right", ""))
	diff := body["version_diffs"].([]any)[0].(map[string]any)
	added := diff["added_changes"].([]any)
	removed := diff["removed_changes"].([]any)
	changed := diff["changed_changes"].([]any)
	if len(added) != 1 || added[0].(map[string]any)["title"] != "only-right" {
		t.Fatalf("added = %v, want [only-right]", added)
	}
	if len(removed) != 1 || removed[0].(map[string]any)["title"] != "only-left" {
		t.Fatalf("removed = %v, want [only-left]", removed)
	}
	if len(changed) != 1 {
		t.Fatalf("changed = %v, want exactly the second x pair", changed)
	}
	pair := changed[0].(map[string]any)
	if pair["left"].(map[string]any)["sequence"] != float64(2) ||
		pair["right"].(map[string]any)["sequence"] != float64(3) {
		t.Fatalf("changed pair must align the second x entries: %v", pair)
	}
}

// When one side repeats a title more often, every surplus entry shows up.
func TestCompareRepeatedTitleSurplusCountsEveryEntry(t *testing.T) {
	router, deps := newTestRouterWithDeps(t)
	legacyEnv(t, deps, "a")
	legacyEnv(t, deps, "b")
	legacyDuplicateRecord(t, deps, "a", "1.0.0", "allowed", "0.9.0",
		dupEntry(1, "feature", "x", "one"),
		dupEntry(2, "feature", "x", "two"),
		dupEntry(3, "feature", "x", "three"))
	legacyDuplicateRecord(t, deps, "b", "1.0.0", "allowed", "0.9.0",
		dupEntry(1, "feature", "x", "one"))

	body := decodeBody(t, doRequest(t, router, http.MethodGet,
		"/api/v1/compare?left=a&right=b", ""))
	diff := body["version_diffs"].([]any)[0].(map[string]any)
	removed := diff["removed_changes"].([]any)
	if len(removed) != 2 {
		t.Fatalf("removed = %v, want both surplus x entries", removed)
	}
	if got := entryViewsTitled(removed); got[0] != "x" || got[1] != "x" {
		t.Fatalf("removed titles = %v, want x,x", got)
	}
	if seqs := removed[0].(map[string]any)["sequence"]; seqs != float64(2) {
		t.Fatalf("first surplus sequence = %v, want 2", seqs)
	}
}

// Repeated queries produce byte-identical output, sorted by title then
// sequence.
func TestCompareRepeatedTitlesAreStable(t *testing.T) {
	router, deps := newTestRouterWithDeps(t)
	legacyEnv(t, deps, "a")
	legacyEnv(t, deps, "b")
	legacyDuplicateRecord(t, deps, "a", "1.0.0", "allowed", "0.9.0",
		dupEntry(3, "feature", "zeta", "d"),
		dupEntry(1, "fix", "alpha", "d1"),
		dupEntry(2, "feature", "alpha", "d2"))
	legacyDuplicateRecord(t, deps, "b", "1.0.0", "allowed", "0.9.0",
		dupEntry(9, "fix", "alpha", "other1"),
		dupEntry(5, "feature", "alpha", "other2"),
		dupEntry(1, "feature", "zeta", "d"))
	target := "/api/v1/compare?left=a&right=b"
	first := doRequest(t, router, http.MethodGet, target, "").Body.String()
	second := doRequest(t, router, http.MethodGet, target, "").Body.String()
	if stripComparedAt(first) != stripComparedAt(second) {
		t.Fatalf("unstable output:\n%s\n%s", first, second)
	}
	body := decodeBody(t, doRequest(t, router, http.MethodGet, target, ""))
	changed := body["version_diffs"].([]any)[0].(map[string]any)["changed_changes"].([]any)
	if len(changed) != 3 {
		t.Fatalf("changed = %v, want both alpha pairs plus zeta", changed)
	}
	if changed[0].(map[string]any)["left"].(map[string]any)["sequence"] != float64(1) ||
		changed[1].(map[string]any)["left"].(map[string]any)["sequence"] != float64(2) {
		t.Fatalf("changed alpha pairs must order by left sequence: %v", changed)
	}
	if changed[2].(map[string]any)["left"].(map[string]any)["title"] != "zeta" {
		t.Fatalf("changed order = %v, want alpha,alpha,zeta by title", changed)
	}
}

func stripComparedAt(raw string) string {
	return strings.SplitN(raw, `"compared_at"`, 2)[0]
}
