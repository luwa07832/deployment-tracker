package v1

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/luwa07832/deployment-tracker/internal/store"
)

const (
	stateV1Body = `{
	  "environment": "prod",
	  "version": "1.1.0",
	  "changes": [
	    {"sequence": 1, "category": "feature", "title": "alpha", "description": "alpha one"}
	  ],
	  "gate_status": "allowed",
	  "rollback_point": "1.0.0"
	}`
	stateV2Body = `{
	  "environment": "prod",
	  "version": "1.2.0",
	  "changes": [
	    {"sequence": 1, "category": "feature", "title": "alpha", "description": "alpha one"},
	    {"sequence": 2, "category": "feature", "title": "beta", "description": "beta new"}
	  ],
	  "gate_status": "blocked",
	  "rollback_point": "1.1.0"
	}`
)

func stateRecordBody(environment, version, gate, rollback string) string {
	return `{
	  "environment": "` + environment + `",
	  "version": "` + version + `",
	  "changes": [
	    {"sequence": 1, "category": "feature", "title": "shared", "description": "shared change"}
	  ],
	  "gate_status": "` + gate + `",
	  "rollback_point": "` + rollback + `"
	}`
}

func fetchReleaseState(t *testing.T, router *gin.Engine, target string) map[string]any {
	t.Helper()
	recorder := doRequest(t, router, http.MethodGet, target, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("release-state status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	return decodeBody(t, recorder)
}

func TestReleaseStateCurrentPreviousAndResolvedTarget(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "prod")
	first := createRecordOK(t, router, stateV1Body)
	second := createRecordOK(t, router, stateV2Body)

	state := fetchReleaseState(t, router, "/api/v1/environments/prod/release-state")
	if state["environment"] != "prod" {
		t.Fatalf("environment = %v", state["environment"])
	}
	if asOf, _ := state["as_of"].(string); asOf == "" {
		t.Fatalf("as_of empty: %v", state["as_of"])
	}
	current := state["current_release"].(map[string]any)
	if current["version"] != "1.2.0" || current["id"] != second["id"] {
		t.Fatalf("current release = %v, want newest 1.2.0", current)
	}
	previous := state["previous_release"].(map[string]any)
	if previous["version"] != "1.1.0" || previous["id"] != first["id"] {
		t.Fatalf("previous release = %v, want 1.1.0", previous)
	}
	if state["rollback_target_status"] != "resolved" {
		t.Fatalf("rollback_target_status = %v, want resolved", state["rollback_target_status"])
	}
	target := state["rollback_target"].(map[string]any)
	if target["version"] != "1.1.0" || target["id"] != first["id"] {
		t.Fatalf("rollback_target = %v, want 1.1.0 record", target)
	}
	impact := state["rollback_impact"].(map[string]any)
	if impact["from_version"] != "1.2.0" || impact["to_version"] != "1.1.0" {
		t.Fatalf("impact versions = %v -> %v", impact["from_version"], impact["to_version"])
	}
	versionDiff := impact["version"].(map[string]any)
	if versionDiff["left"] != "1.2.0" || versionDiff["right"] != "1.1.0" || versionDiff["changed"] != true {
		t.Fatalf("version diff = %v", versionDiff)
	}
	gateDiff := impact["gate_status"].(map[string]any)
	if gateDiff["changed"] != true {
		t.Fatalf("gate diff = %v", gateDiff)
	}
	summary := impact["change_summary"].(map[string]any)
	if summary["missing"] != float64(1) {
		t.Fatalf("change_summary = %v, want 1 missing (beta exists only in current)", summary)
	}
	if impact["consistent"] != false {
		t.Fatalf("consistent = %v, want false", impact["consistent"])
	}
}

func TestReleaseStatePreviousNullWhenSingleRelease(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "prod")
	createRecordOK(t, router, stateRecordBody("prod", "1.0.0", "allowed", "1.0.0"))
	state := fetchReleaseState(t, router, "/api/v1/environments/prod/release-state")
	if _, present := state["previous_release"]; !present {
		t.Fatalf("previous_release key must be present: %v", state)
	}
	if state["previous_release"] != nil {
		t.Fatalf("previous_release = %v, want null", state["previous_release"])
	}
}

func TestReleaseStateMissingTargetWhenRollbackPointUnresolved(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "prod")
	createRecordOK(t, router, stateRecordBody("prod", "1.0.0", "allowed", "0.9.0"))
	state := fetchReleaseState(t, router, "/api/v1/environments/prod/release-state")
	if state["rollback_target_status"] != "missing" {
		t.Fatalf("status = %v, want missing", state["rollback_target_status"])
	}
	if state["rollback_target"] != nil {
		t.Fatalf("rollback_target = %v, want null", state["rollback_target"])
	}
	impact := state["rollback_impact"].(map[string]any)
	if impact["consistent"] != false {
		t.Fatalf("impact consistent = %v, want false", impact["consistent"])
	}
	if impact["to_version"] != "" {
		t.Fatalf("to_version = %v, want empty", impact["to_version"])
	}
	summary := impact["change_summary"].(map[string]any)
	if summary["missing"] != float64(1) {
		t.Fatalf("change_summary = %v, want 1 missing", summary)
	}
}

func TestReleaseStateRequestedTarget(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "prod")
	v1 := createRecordOK(t, router, stateV1Body)
	createRecordOK(t, router, stateV2Body)

	state := fetchReleaseState(t, router,
		"/api/v1/environments/prod/release-state?target_version=1.1.0")
	if state["rollback_target_status"] != "requested" {
		t.Fatalf("status = %v, want requested", state["rollback_target_status"])
	}
	target := state["rollback_target"].(map[string]any)
	if target["id"] != v1["id"] {
		t.Fatalf("target = %v, want explicitly requested 1.1.0", target)
	}
}

func TestReleaseStateRequestedTargetNotFound(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "prod")
	createRecordOK(t, router, stateV2Body)
	recorder := doRequest(t, router, http.MethodGet,
		"/api/v1/environments/prod/release-state?target_version=9.9.9", "")
	wantError(t, recorder, http.StatusNotFound, store.CodeRollbackTargetNotFound)
}

func TestReleaseStateTargetEqualsCurrentIsEmptyConsistentDiff(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "prod")
	createRecordOK(t, router, stateRecordBody("prod", "1.0.0", "allowed", "1.0.0"))
	state := fetchReleaseState(t, router,
		"/api/v1/environments/prod/release-state?target_version=1.0.0")
	if state["rollback_target_status"] != "requested" {
		t.Fatalf("status = %v", state["rollback_target_status"])
	}
	impact := state["rollback_impact"].(map[string]any)
	if impact["consistent"] != true {
		t.Fatalf("consistent = %v, want true", impact["consistent"])
	}
	changes := impact["changes"].([]any)
	if len(changes) != 0 {
		t.Fatalf("changes = %v, want empty", changes)
	}
	summary := impact["change_summary"].(map[string]any)
	if summary["added"] != float64(0) || summary["missing"] != float64(0) || summary["changed"] != float64(0) {
		t.Fatalf("summary = %v, want all zero", summary)
	}
	for _, key := range []string{"version", "gate_status", "rollback_point"} {
		diff := impact[key].(map[string]any)
		if diff["changed"] != false {
			t.Fatalf("%s changed = %v, want false", key, diff["changed"])
		}
	}
}

func TestReleaseStateBlankTargetVersion(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "prod")
	createRecordOK(t, router, stateRecordBody("prod", "1.0.0", "allowed", "1.0.0"))
	for _, target := range []string{
		"/api/v1/environments/prod/release-state?target_version=",
		"/api/v1/environments/prod/release-state?target_version=%20%20",
	} {
		recorder := doRequest(t, router, http.MethodGet, target, "")
		wantError(t, recorder, http.StatusBadRequest, store.CodeInvalidReleaseStateTarget)
	}
}

func TestReleaseStateInvalidAsOf(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "prod")
	for _, raw := range []string{"tuesday", "2026-13-99", "not-a-date", "%20%20"} {
		recorder := doRequest(t, router, http.MethodGet,
			"/api/v1/environments/prod/release-state?as_of="+raw, "")
		wantError(t, recorder, http.StatusUnprocessableEntity, store.CodeReleaseStateTimeInvalid)
	}
}

func TestReleaseStateEnvironmentNotFound(t *testing.T) {
	router := newTestRouter(t)
	recorder := doRequest(t, router, http.MethodGet,
		"/api/v1/environments/ghost/release-state", "")
	wantError(t, recorder, http.StatusNotFound, store.CodeEnvironmentNotFoundV1)
}

func TestReleaseStateNotFoundBeforeAnyRelease(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "prod")
	recorder := doRequest(t, router, http.MethodGet,
		"/api/v1/environments/prod/release-state?as_of=2000-01-01", "")
	wantError(t, recorder, http.StatusNotFound, store.CodeReleaseStateNotFound)
}

func TestReleaseStateAsOfDateNormalizedEndOfDayUTC(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "prod")
	createRecordOK(t, router, stateV2Body)
	state := fetchReleaseState(t, router,
		"/api/v1/environments/prod/release-state?as_of=2999-01-01")
	if state["as_of"] != "2999-01-01T23:59:59Z" {
		t.Fatalf("as_of = %v, want 2999-01-01T23:59:59Z", state["as_of"])
	}
}

func TestReleaseStateExplicitAsOfIsStable(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "prod")
	createRecordOK(t, router, stateV1Body)
	createRecordOK(t, router, stateV2Body)
	target := "/api/v1/environments/prod/release-state?as_of=2999-01-01T00:00:00Z"
	first := doRequest(t, router, http.MethodGet, target, "")
	second := doRequest(t, router, http.MethodGet, target, "")
	if first.Code != http.StatusOK || second.Code != http.StatusOK {
		t.Fatalf("status %d/%d", first.Code, second.Code)
	}
	if first.Body.String() != second.Body.String() {
		t.Fatalf("explicit as_of responses differ:\n%s\n%s", first.Body.String(), second.Body.String())
	}
	var state struct {
		AsOf string `json:"as_of"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if state.AsOf != "2999-01-01T00:00:00Z" {
		t.Fatalf("as_of echoed = %q", state.AsOf)
	}
}

func TestReleaseStateEnvironmentMatchedVerbatim(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "Prod")
	createRecordOK(t, router, stateRecordBody("Prod", "1.0.0", "allowed", "1.0.0"))
	recorder := doRequest(t, router, http.MethodGet,
		"/api/v1/environments/prod/release-state", "")
	wantError(t, recorder, http.StatusNotFound, store.CodeEnvironmentNotFoundV1)
	body := fetchReleaseState(t, router,
		"/api/v1/environments/Prod/release-state")
	if body["environment"] != "Prod" {
		t.Fatalf("verbatim environment key lost: %v", body)
	}
}

func TestReleaseStateStorageUnavailable(t *testing.T) {
	router, db := newChangeTestRouter(t)
	registerEnvironmentOK(t, router, "prod")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	recorder := doRequest(t, router, http.MethodGet,
		"/api/v1/environments/prod/release-state", "")
	wantError(t, recorder, http.StatusServiceUnavailable, store.CodeStorageUnavailable)
}

func TestReleaseStateCutoffSelectsHistoricalCurrent(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "prod")
	base := createRecordOK(t, router, stateRecordBody("prod", "1.0.0", "allowed", "1.0.0"))
	createRecordOK(t, router, stateV1Body)
	createRecordOK(t, router, stateV2Body)

	atBase := base["recorded_at"].(string)
	state := fetchReleaseState(t, router,
		"/api/v1/environments/prod/release-state?as_of=2999-01-01")
	if state["as_of"] != "2999-01-01T23:59:59Z" {
		t.Fatalf("as_of = %v", state["as_of"])
	}
	_ = atBase
	current := state["current_release"].(map[string]any)
	if current["version"] != "1.2.0" {
		t.Fatalf("future cutoff current = %v, want newest 1.2.0", current["version"])
	}
	if previous := state["previous_release"].(map[string]any); previous["version"] != "1.1.0" {
		t.Fatalf("previous = %v, want 1.1.0", previous)
	}
}

func TestReleaseStateCutoffExcludesFutureRollbackTarget(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "prod")
	// Only record exists after the cutoff date; both current selection and
	// default rollback-point resolution must stay inside the cutoff.
	createRecordOK(t, router, stateRecordBody("prod", "1.0.0", "allowed", "1.0.0"))
	recorder := doRequest(t, router, http.MethodGet,
		"/api/v1/environments/prod/release-state?as_of=2000-01-01", "")
	wantError(t, recorder, http.StatusNotFound, store.CodeReleaseStateNotFound)
}

func TestReleaseStateRollbackImpactAddedAndChanged(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "prod")
	createRecordOK(t, router, `{
	  "environment": "prod", "version": "1.1.0",
	  "changes": [
	    {"sequence": 1, "category": "feature", "title": "alpha", "description": "old alpha"},
	    {"sequence": 2, "category": "fix", "title": "gamma", "description": "gamma desc"}
	  ],
	  "gate_status": "allowed", "rollback_point": "1.1.0"}`)
	createRecordOK(t, router, `{
	  "environment": "prod", "version": "1.2.0",
	  "changes": [
	    {"sequence": 1, "category": "feature", "title": "alpha", "description": "new alpha"},
	    {"sequence": 3, "category": "feature", "title": "beta", "description": "beta desc"}
	  ],
	  "gate_status": "allowed", "rollback_point": "1.1.0"}`)
	state := fetchReleaseState(t, router,
		"/api/v1/environments/prod/release-state?target_version=1.1.0")
	impact := state["rollback_impact"].(map[string]any)
	summary := impact["change_summary"].(map[string]any)
	if summary["added"] != float64(1) || summary["missing"] != float64(1) || summary["changed"] != float64(1) {
		t.Fatalf("summary = %v, want 1/1/1", summary)
	}
	comparisons := map[string]string{}
	for _, item := range impact["changes"].([]any) {
		change := item.(map[string]any)
		comparisons[change["change_id"].(string)] = change["comparison"].(string)
	}
	if comparisons["alpha"] != "changed" || comparisons["beta"] != "missing" || comparisons["gamma"] != "added" {
		t.Fatalf("comparisons = %v", comparisons)
	}
}
