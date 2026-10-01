package api

import (
	"database/sql"
	"net/http"
	"strings"
	"testing"

	"github.com/luwa07832/deployment-tracker/internal/store"
)

func TestCompareEnvironments(t *testing.T) {
	router, _ := newReleaseTestRouter(t)
	createReleaseOK(t, router, `{"environment":"staging","version":"1.0.0","changes":["a","b"],"gate_status":"allowed","rollback_point":"0.9.0"}`)
	createReleaseOK(t, router, `{"environment":"staging","version":"1.1.0","changes":["a","b","c"],"gate_status":"pending","rollback_point":"1.0.0"}`)
	createReleaseOK(t, router, `{"environment":"staging","version":"1.2.0","changes":["a"],"gate_status":"blocked","rollback_point":"1.1.0"}`)
	createReleaseOK(t, router, `{"environment":"prod","version":"1.0.0","changes":["a","d"],"gate_status":"blocked","rollback_point":"0.8.0"}`)
	createReleaseOK(t, router, `{"environment":"prod","version":"1.3.0","changes":["a"],"gate_status":"allowed","rollback_point":"1.0.0"}`)

	recorder := doRequest(t, router, http.MethodGet, "/compare?left=staging&right=prod", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	body := decodeBody(t, recorder)
	if body["left"] != "staging" || body["right"] != "prod" {
		t.Fatalf("left/right = %v/%v", body["left"], body["right"])
	}
	wantStrings := map[string]string{
		"left_versions":         `["1.0.0","1.1.0","1.2.0"]`,
		"right_versions":        `["1.0.0","1.3.0"]`,
		"common_versions":       `["1.0.0"]`,
		"only_left_versions":    `["1.1.0","1.2.0"]`,
		"only_right_versions":   `["1.3.0"]`,
		"left_rollback_points":  `["0.9.0","1.0.0","1.1.0"]`,
		"right_rollback_points": `["0.8.0","1.0.0"]`,
	}
	raw := recorder.Body.String()
	for key, want := range wantStrings {
		if !strings.Contains(raw, `"`+key+`":`+want) {
			t.Fatalf("body missing %s=%s\nbody: %s", key, want, raw)
		}
	}
	diffs := body["version_diffs"].([]any)
	if len(diffs) != 1 {
		t.Fatalf("version_diffs = %v, want 1 entry", diffs)
	}
	diff := diffs[0].(map[string]any)
	if diff["version"] != "1.0.0" {
		t.Fatalf("diff version = %v, want 1.0.0", diff["version"])
	}
	added := diff["added_changes"].([]any)
	removed := diff["removed_changes"].([]any)
	if len(added) != 1 || added[0] != "d" {
		t.Fatalf("added = %v, want [d]", added)
	}
	if len(removed) != 1 || removed[0] != "b" {
		t.Fatalf("removed = %v, want [b]", removed)
	}
	if diff["left_gate_status"] != "allowed" || diff["right_gate_status"] != "blocked" {
		t.Fatalf("gate statuses = %v/%v", diff["left_gate_status"], diff["right_gate_status"])
	}
	if diff["gate_status_changed"] != true {
		t.Fatalf("gate_status_changed = %v, want true", diff["gate_status_changed"])
	}
}

func TestCompareEnvironmentsWithCutoff(t *testing.T) {
	router, _ := newReleaseTestRouter(t)
	createReleaseOK(t, router, `{"environment":"a","version":"1.0.0","changes":["x"],"gate_status":"allowed","rollback_point":"0.9.0"}`)
	createReleaseOK(t, router, `{"environment":"a","version":"2.0.0","changes":["x"],"gate_status":"allowed","rollback_point":"1.0.0"}`)
	createReleaseOK(t, router, `{"environment":"b","version":"1.0.0","changes":["x"],"gate_status":"allowed","rollback_point":"0.9.0"}`)
	createReleaseOK(t, router, `{"environment":"b","version":"1.5.0","changes":["x"],"gate_status":"allowed","rollback_point":"1.0.0"}`)

	recorder := doRequest(t, router, http.MethodGet, "/compare?left=a&right=b&until=1.5.0", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	raw := recorder.Body.String()
	for _, want := range []string{
		`"until":"1.5.0"`,
		`"left_versions":["1.0.0"]`,
		`"right_versions":["1.0.0","1.5.0"]`,
		`"common_versions":["1.0.0"]`,
		`"only_left_versions":[]`,
		`"only_right_versions":["1.5.0"]`,
	} {
		if !strings.Contains(raw, want) {
			t.Fatalf("body missing %s\nbody: %s", want, raw)
		}
	}
}

func TestCompareEnvironmentsErrors(t *testing.T) {
	router, _ := newReleaseTestRouter(t)
	createReleaseOK(t, router, `{"environment":"a","version":"1.0.0","changes":["x"],"gate_status":"allowed","rollback_point":"0.9.0"}`)

	wantError(t, doRequest(t, router, http.MethodGet, "/compare?left=a&right=a", ""), http.StatusConflict, store.CodeComparisonConflict)
	wantError(t, doRequest(t, router, http.MethodGet, "/compare?left=a&right=nowhere", ""), http.StatusNotFound, store.CodeEnvironmentNotFound)
	wantError(t, doRequest(t, router, http.MethodGet, "/compare?left=nowhere&right=a", ""), http.StatusNotFound, store.CodeEnvironmentNotFound)
	wantError(t, doRequest(t, router, http.MethodGet, "/compare?left=a", ""), http.StatusBadRequest, store.CodeInvalidRequest)
}

func TestCompareEnvironmentsEmptyIntersection(t *testing.T) {
	router, _ := newReleaseTestRouter(t)
	createReleaseOK(t, router, `{"environment":"a","version":"1.0.0","changes":["x"],"gate_status":"allowed","rollback_point":"0.9.0"}`)
	createReleaseOK(t, router, `{"environment":"b","version":"2.0.0","changes":["y"],"gate_status":"blocked","rollback_point":"1.0.0"}`)

	recorder := doRequest(t, router, http.MethodGet, "/compare?left=a&right=b", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	raw := recorder.Body.String()
	for _, want := range []string{
		`"common_versions":[]`,
		`"version_diffs":[]`,
		`"only_left_versions":["1.0.0"]`,
		`"only_right_versions":["2.0.0"]`,
	} {
		if !strings.Contains(raw, want) {
			t.Fatalf("body missing %s\nbody: %s", want, raw)
		}
	}
}

// TestCompareUsesLatestRecordPerVersion seeds duplicate legacy rows for one
// version and checks the comparison uses the one registered last.
func TestCompareUsesLatestRecordPerVersion(t *testing.T) {
	router, path := newReleaseTestRouter(t)
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO deployments (name, environment, version, changes_json, gate_status, rollback_point)
		VALUES ('old', 'a', '1.0.0', '["old-change"]', 'blocked', '0.1.0')`); err != nil {
		t.Fatalf("seed old row: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO deployments (name, environment, version, changes_json, gate_status, rollback_point)
		VALUES ('new', 'a', '1.0.0', '["new-change"]', 'allowed', '0.2.0')`); err != nil {
		t.Fatalf("seed new row: %v", err)
	}
	raw.Close()
	createReleaseOK(t, router, `{"environment":"b","version":"1.0.0","changes":["new-change"],"gate_status":"allowed","rollback_point":"0.2.0"}`)

	recorder := doRequest(t, router, http.MethodGet, "/compare?left=a&right=b", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	body := decodeBody(t, recorder)
	diffs := body["version_diffs"].([]any)
	if len(diffs) != 1 {
		t.Fatalf("version_diffs = %v, want 1 entry", diffs)
	}
	diff := diffs[0].(map[string]any)
	if diff["left_gate_status"] != "allowed" {
		t.Fatalf("left gate = %v, want the later record's allowed", diff["left_gate_status"])
	}
	if added := diff["added_changes"].([]any); len(added) != 0 {
		t.Fatalf("added = %v, want empty because the later record matches", added)
	}
	if !strings.Contains(recorder.Body.String(), `"left_rollback_points":["0.2.0"]`) {
		t.Fatalf("rollback points must come from the later record\nbody: %s", recorder.Body.String())
	}
}

// TestCompareWithLegacyRecords keeps legacy rows (no new fields) comparable:
// they contribute versions but null gate statuses.
func TestCompareWithLegacyRecords(t *testing.T) {
	router, path := newReleaseTestRouter(t)
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO deployments (name, environment, version) VALUES ('legacy', 'old-env', '1.0.0')`); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}
	raw.Close()
	createReleaseOK(t, router, `{"environment":"new-env","version":"1.0.0","changes":["x"],"gate_status":"allowed","rollback_point":"0.9.0"}`)

	recorder := doRequest(t, router, http.MethodGet, "/compare?left=old-env&right=new-env", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	rawBody := recorder.Body.String()
	for _, want := range []string{
		`"common_versions":["1.0.0"]`,
		`"left_gate_status":null`,
		`"right_gate_status":"allowed"`,
		`"gate_status_changed":true`,
		`"left_rollback_points":[]`,
	} {
		if !strings.Contains(rawBody, want) {
			t.Fatalf("body missing %s\nbody: %s", want, rawBody)
		}
	}
}
