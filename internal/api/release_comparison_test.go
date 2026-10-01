package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"

	_ "modernc.org/sqlite"
)

// releaseBody builds a POST /releases body from the comparable facts.
func releaseBody(environment, version, gateStatus, rollbackPoint string, changes ...string) string {
	encoded, err := json.Marshal(map[string]any{
		"environment":    environment,
		"version":        version,
		"changes":        changes,
		"gate_status":    gateStatus,
		"rollback_point": rollbackPoint,
	})
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

// setupComparisonRouter returns a router with matching 1.2.0 releases on
// staging and prod plus their rollback-target histories.
func setupComparisonRouter(t *testing.T) *gin.Engine {
	t.Helper()
	router, _ := newReleaseTestRouter(t)
	createReleaseOK(t, router, releaseBody("staging", "0.8.0", "allowed", "0.8.0", "base"))
	createReleaseOK(t, router, releaseBody("prod", "0.8.0", "allowed", "0.8.0", "base"))
	createReleaseOK(t, router, releaseBody("staging", "0.9.0", "allowed", "0.8.0", "base"))
	createReleaseOK(t, router, releaseBody("prod", "0.9.0", "allowed", "0.8.0", "base"))
	createReleaseOK(t, router, releaseBody("staging", "1.0.0", "allowed", "0.9.0", "base", "staging-only-100"))
	createReleaseOK(t, router, releaseBody("prod", "1.0.0", "blocked", "0.9.0", "base", "prod-only-100"))
	createReleaseOK(t, router, releaseBody("staging", "1.2.0", "allowed", "1.0.0", "add login", "fix logout"))
	createReleaseOK(t, router, releaseBody("prod", "1.2.0", "blocked", "1.0.0", "add login", "fix logout"))
	return router
}

func TestReleaseComparisonConsistent(t *testing.T) {
	router := setupComparisonRouter(t)
	// Two releases with identical versions, gates, changes and rollback points.
	recorder := doRequest(t, router, http.MethodGet,
		"/release-comparisons?left=staging&left_version=0.9.0&right=prod&right_version=0.9.0", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	body := decodeBody(t, recorder)
	if body["consistent"] != true {
		t.Fatalf("consistent = %v, want true (body %s)", body["consistent"], recorder.Body.String())
	}
	changeDiffs, _ := body["change_diffs"].([]any)
	if len(changeDiffs) != 0 {
		t.Fatalf("change_diffs = %v, want empty array", changeDiffs)
	}
	for _, field := range []string{"version", "gate_status", "rollback_point"} {
		section, _ := body[field].(map[string]any)
		if section["consistent"] != true {
			t.Fatalf("%s.consistent = %v, want true", field, section["consistent"])
		}
	}
}

func TestReleaseComparisonClassifiesChangesAndFacts(t *testing.T) {
	router := setupComparisonRouter(t)
	recorder := doRequest(t, router, http.MethodGet,
		"/release-comparisons?left=staging&left_version=1.0.0&right=prod&right_version=1.0.0", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	body := decodeBody(t, recorder)
	if body["consistent"] != false {
		t.Fatalf("consistent = %v, want false", body["consistent"])
	}
	diffs := body["change_diffs"].([]any)
	if len(diffs) != 2 {
		t.Fatalf("change_diffs len = %d, want 2 (body %s)", len(diffs), recorder.Body.String())
	}
	first, _ := diffs[0].(map[string]any)
	second, _ := diffs[1].(map[string]any)
	// Stable lexical order by the change identifier.
	if first["change_id"] != "prod-only-100" || first["kind"] != "added" {
		t.Fatalf("first diff = %v, want added prod-only-100", first)
	}
	if first["present_on_left"] != false || first["present_on_right"] != true || first["content_status"] != "absent" {
		t.Fatalf("added diff presence/status wrong: %v", first)
	}
	if second["change_id"] != "staging-only-100" || second["kind"] != "missing" {
		t.Fatalf("second diff = %v, want missing staging-only-100", second)
	}
	if second["present_on_left"] != true || second["present_on_right"] != false {
		t.Fatalf("missing diff presence wrong: %v", second)
	}
	if second["summary"] != "staging-only-100" {
		t.Fatalf("summary = %v, want change id", second["summary"])
	}
	gate, _ := body["gate_status"].(map[string]any)
	if gate["left"] != "allowed" || gate["right"] != "blocked" || gate["consistent"] != false {
		t.Fatalf("gate_status = %v", gate)
	}
	versionSection, _ := body["version"].(map[string]any)
	if versionSection["consistent"] != true {
		t.Fatalf("version.consistent = %v, want true for same version", versionSection["consistent"])
	}
	rollback, _ := body["rollback_point"].(map[string]any)
	if rollback["consistent"] != true {
		t.Fatalf("rollback_point.consistent = %v, want true: same identifier and same target content", rollback["consistent"])
	}
}

func TestReleaseComparisonDifferentVersionsAndRollbackTargets(t *testing.T) {
	router := setupComparisonRouter(t)
	recorder := doRequest(t, router, http.MethodGet,
		"/release-comparisons?left=staging&left_version=1.2.0&right=prod&right_version=1.0.0", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	body := decodeBody(t, recorder)
	versionSection, _ := body["version"].(map[string]any)
	if versionSection["left"] != "1.2.0" || versionSection["right"] != "1.0.0" || versionSection["consistent"] != false {
		t.Fatalf("version section = %v", versionSection)
	}
	rollback, _ := body["rollback_point"].(map[string]any)
	// 1.0.0 vs 1.2.0 differ in identifier; the targets they point at also
	// differ in gate status and change content.
	if rollback["consistent"] != false {
		t.Fatalf("rollback_point.consistent = %v, want false", rollback["consistent"])
	}
	leftSide, _ := rollback["left"].(map[string]any)
	rightSide, _ := rollback["right"].(map[string]any)
	if leftSide["identifier"] != "1.0.0" || rightSide["identifier"] != "0.9.0" {
		t.Fatalf("rollback identifiers = %v / %v", leftSide["identifier"], rightSide["identifier"])
	}
	leftTarget, _ := leftSide["target_release"].(map[string]any)
	if leftTarget["version"] != "1.0.0" || leftTarget["environment"] != "staging" {
		t.Fatalf("left target = %v", leftTarget)
	}
	rightTarget, _ := rightSide["target_release"].(map[string]any)
	if rightTarget["gate_status"] != "allowed" {
		t.Fatalf("right target = %v", rightTarget)
	}
}

func TestReleaseComparisonRollbackTargetContentDiffers(t *testing.T) {
	router, _ := newReleaseTestRouter(t)
	// Same rollback point identifier on both sides, but the release object it
	// identifies differs (prod's 1.0.0 was blocked), so the points are unequal.
	createReleaseOK(t, router, releaseBody("staging", "1.0.0", "allowed", "0.9.0", "a"))
	createReleaseOK(t, router, releaseBody("prod", "1.0.0", "blocked", "0.9.0", "a"))
	createReleaseOK(t, router, releaseBody("staging", "0.9.0", "allowed", "0.8.0", "base"))
	createReleaseOK(t, router, releaseBody("prod", "0.9.0", "blocked", "0.8.0", "base"))
	createReleaseOK(t, router, releaseBody("staging", "1.2.0", "allowed", "1.0.0", "a"))
	createReleaseOK(t, router, releaseBody("prod", "1.2.0", "allowed", "1.0.0", "a"))
	recorder := doRequest(t, router, http.MethodGet,
		"/release-comparisons?left=staging&left_version=1.2.0&right=prod&right_version=1.2.0", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	body := decodeBody(t, recorder)
	rollback, _ := body["rollback_point"].(map[string]any)
	if rollback["consistent"] != false {
		t.Fatalf("rollback_point.consistent = %v, want false because target content differs", rollback["consistent"])
	}
}

func TestReleaseComparisonRepeatable(t *testing.T) {
	router := setupComparisonRouter(t)
	target := "/release-comparisons?left=staging&left_version=1.2.0&right=prod&right_version=1.2.0"
	first := doRequest(t, router, http.MethodGet, target, "")
	second := doRequest(t, router, http.MethodGet, target, "")
	if first.Body.String() != second.Body.String() {
		t.Fatalf("repeated queries differ:\n%s\n%s", first.Body.String(), second.Body.String())
	}
	body := decodeBody(t, first)
	if body["consistent"] != false {
		t.Fatalf("consistent = %v, want false: gates differ", body["consistent"])
	}
	diffs := body["change_diffs"].([]any)
	if len(diffs) != 0 {
		t.Fatalf("change_diffs = %v, want empty: change entries identical", diffs)
	}
}

func TestReleaseComparisonSameEnvironmentDifferentVersions(t *testing.T) {
	router := setupComparisonRouter(t)
	// Comparing two historical versions within one environment is allowed.
	recorder := doRequest(t, router, http.MethodGet,
		"/release-comparisons?left=prod&left_version=0.9.0&right=prod&right_version=1.2.0", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	body := decodeBody(t, recorder)
	if body["left_environment"] != "prod" || body["right_environment"] != "prod" {
		t.Fatalf("environments = %v / %v", body["left_environment"], body["right_environment"])
	}
}

func TestReleaseComparisonMissingParameters(t *testing.T) {
	router := setupComparisonRouter(t)
	recorder := doRequest(t, router, http.MethodGet, "/release-comparisons?left=staging&right=prod", "")
	wantError(t, recorder, http.StatusBadRequest, "invalid_request")
}

func TestReleaseComparisonEnvironmentNotFound(t *testing.T) {
	router := setupComparisonRouter(t)
	recorder := doRequest(t, router, http.MethodGet,
		"/release-comparisons?left=ghost&left_version=1.0.0&right=prod&right_version=1.0.0", "")
	wantError(t, recorder, http.StatusNotFound, "ReleaseComparisonEnvironmentNotFound")
	// Left valid, right unknown.
	recorder = doRequest(t, router, http.MethodGet,
		"/release-comparisons?left=prod&left_version=1.0.0&right=ghost&right_version=1.0.0", "")
	wantError(t, recorder, http.StatusNotFound, "ReleaseComparisonEnvironmentNotFound")
}

func TestReleaseComparisonVersionNotFound(t *testing.T) {
	router := setupComparisonRouter(t)
	recorder := doRequest(t, router, http.MethodGet,
		"/release-comparisons?left=staging&left_version=9.9.9&right=prod&right_version=1.0.0", "")
	wantError(t, recorder, http.StatusNotFound, "ReleaseComparisonVersionNotFound")
	recorder = doRequest(t, router, http.MethodGet,
		"/release-comparisons?left=staging&left_version=1.0.0&right=prod&right_version=9.9.9", "")
	wantError(t, recorder, http.StatusNotFound, "ReleaseComparisonVersionNotFound")
	// Exact stored value required: "1.2" must not fuzzy-match "1.2.0".
	recorder = doRequest(t, router, http.MethodGet,
		"/release-comparisons?left=staging&left_version=1.2&right=prod&right_version=1.2.0", "")
	wantError(t, recorder, http.StatusNotFound, "ReleaseComparisonVersionNotFound")
}

func TestReleaseComparisonDataIncompleteLegacyRecord(t *testing.T) {
	router, path := newReleaseTestRouter(t)
	createReleaseOK(t, router, releaseBody("prod", "0.9.0", "allowed", "0.8.0", "base"))
	// Legacy row predating changes/gate_status/rollback_point tracking.
	raw, err := sql.Open("sqlite", filepath.Join(path))
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`INSERT INTO deployments (name, environment, version) VALUES ('legacy', 'staging', '1.0.0')`); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}
	recorder := doRequest(t, router, http.MethodGet,
		"/release-comparisons?left=staging&left_version=1.0.0&right=prod&right_version=0.9.0", "")
	wantError(t, recorder, http.StatusUnprocessableEntity, "ReleaseComparisonDataIncomplete")
}

func TestReleaseComparisonDataIncompleteDanglingRollbackPoint(t *testing.T) {
	router, _ := newReleaseTestRouter(t)
	createReleaseOK(t, router, releaseBody("prod", "1.0.0", "allowed", "0.9.0", "a"))
	createReleaseOK(t, router, releaseBody("prod", "0.9.0", "allowed", "0.8.0", "base"))
	// staging 1.0.0 points at 0.9.0, but staging has no recorded 0.9.0.
	createReleaseOK(t, router, releaseBody("staging", "1.0.0", "allowed", "0.9.0", "a"))
	recorder := doRequest(t, router, http.MethodGet,
		"/release-comparisons?left=staging&left_version=1.0.0&right=prod&right_version=1.0.0", "")
	wantError(t, recorder, http.StatusUnprocessableEntity, "ReleaseComparisonDataIncomplete")
}
