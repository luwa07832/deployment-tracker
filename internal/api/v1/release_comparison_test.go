package v1

import (
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
)

func sprintfBody(environment, version, alphaDesc, gate, rollback string) string {
	return "{\n" +
		"  \"environment\": \"" + environment + "\",\n" +
		"  \"version\": \"" + version + "\",\n" +
		"  \"changes\": [\n" +
		"    {\"sequence\": 1, \"category\": \"feature\", \"title\": \"alpha\", \"description\": \"" + alphaDesc + "\"},\n" +
		"    {\"sequence\": 2, \"category\": \"fix\", \"title\": \"beta\", \"description\": \"kept beta\"}\n" +
		"  ],\n" +
		"  \"gate_status\": \"" + gate + "\",\n" +
		"  \"rollback_point\": \"" + rollback + "\"\n}"
}
func seedComparisonStack(t *testing.T, router *gin.Engine) {
	t.Helper()
	registerEnvironmentOK(t, router, "dev")
	registerEnvironmentOK(t, router, "staging")
	registerEnvironmentOK(t, router, "prod")
	// Rollback bases: dev 0.9.0 and 1.0.0; staging 0.9.0 and 1.0.0; prod 1.0.0.
	createRecordOK(t, router, `{
	  "environment": "dev", "version": "0.9.0",
	  "changes": [{"sequence": 1, "category": "feature", "title": "base", "description": "old base"}],
	  "gate_status": "allowed", "rollback_point": "0.9.0"}`)
	createRecordOK(t, router, `{
	  "environment": "staging", "version": "0.9.0",
	  "changes": [{"sequence": 1, "category": "feature", "title": "base", "description": "old base"}],
	  "gate_status": "allowed", "rollback_point": "0.9.0"}`)
	createRecordOK(t, router, `{
	  "environment": "prod", "version": "1.0.0",
	  "changes": [{"sequence": 1, "category": "feature", "title": "beta", "description": "kept beta"},
	              {"sequence": 2, "category": "fix", "title": "gamma", "description": "only on prod old"}],
	  "gate_status": "blocked", "rollback_point": "1.0.0"}`)
	createRecordOK(t, router, `{
	  "environment": "dev", "version": "1.0.0",
	  "changes": [{"sequence": 1, "category": "feature", "title": "base", "description": "base at one"}],
	  "gate_status": "allowed", "rollback_point": "0.9.0"}`)
	createRecordOK(t, router, `{
	  "environment": "staging", "version": "1.0.0",
	  "changes": [{"sequence": 1, "category": "feature", "title": "base", "description": "base at one"}],
	  "gate_status": "allowed", "rollback_point": "0.9.0"}`)
	// Compared releases:
	// dev 2.0.0: alpha + beta; staging 2.0.0: alpha(changed) + beta + gamma(added).
	createRecordOK(t, router, sprintfBody("dev", "2.0.0", "alpha on dev", "allowed", "1.0.0"))
	createRecordOK(t, router, sprintfBody("staging", "2.0.0", "alpha on staging", "allowed", "1.0.0"))
	createRecordOK(t, router, `{
	  "environment": "staging", "version": "2.1.0",
	  "changes": [
	    {"sequence": 1, "category": "feature", "title": "alpha", "description": "alpha on staging"},
	    {"sequence": 2, "category": "fix", "title": "beta", "description": "kept beta"},
	    {"sequence": 3, "category": "feature", "title": "gamma", "description": "new gamma"}
	  ],
	  "gate_status": "blocked", "rollback_point": "1.0.0"}`)
}

func compareOK(t *testing.T, router *gin.Engine, target string) (int, map[string]any) {
	t.Helper()
	recorder := doRequest(t, router, http.MethodGet, target, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("compare status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	return recorder.Code, decodeBody(t, recorder)
}

func TestReleaseComparisonReportsUnifiedDiff(t *testing.T) {
	router := newTestRouter(t)
	seedComparisonStack(t, router)

	_, body := compareOK(t, router,
		"/api/v1/release-comparison?left=dev&left_version=2.0.0&right=staging&right_version=2.1.0")
	if body["left_environment"] != "dev" || body["right_environment"] != "staging" {
		t.Fatalf("unexpected side echo: %v", body)
	}
	versionDiff := body["version"].(map[string]any)
	if versionDiff["changed"] != true || versionDiff["left"] != "2.0.0" || versionDiff["right"] != "2.1.0" {
		t.Fatalf("version diff wrong: %v", versionDiff)
	}
	gate := body["gate_status"].(map[string]any)
	if gate["changed"] != true || gate["left"] != "allowed" || gate["right"] != "blocked" {
		t.Fatalf("gate diff wrong: %v", gate)
	}
	rollback := body["rollback_point"].(map[string]any)
	if rollback["changed"] != false || rollback["target_changed"] != false {
		t.Fatalf("rollback should be consistent (same identifier 1.0.0, same target object): %v", rollback)
	}
	if body["consistent"] != false {
		t.Fatalf("consistent must be false: %v", body)
	}
	totals := body["change_summary"].(map[string]any)
	if totals["added"] != float64(1) || totals["missing"] != float64(0) || totals["changed"] != float64(1) {
		t.Fatalf("change totals wrong: %v", totals)
	}
	changes := body["changes"].([]any)
	if len(changes) != 2 {
		t.Fatalf("want alpha(changed) and gamma(added), got %v", changes)
	}
	alpha := changes[0].(map[string]any)
	gamma := changes[1].(map[string]any)
	if alpha["change_id"] != "alpha" || alpha["comparison"] != "changed" {
		t.Fatalf("first row must be changed alpha, got %v", alpha)
	}
	if alpha["present"].(map[string]any)["left"] != true {
		t.Fatalf("alpha present on both sides: %v", alpha)
	}
	if gamma["change_id"] != "gamma" || gamma["comparison"] != "added" {
		t.Fatalf("second row must be added gamma, got %v", gamma)
	}
	if gamma["present"].(map[string]any)["right"] != true ||
		gamma["present"].(map[string]any)["left"] != false {
		t.Fatalf("gamma presence wrong: %v", gamma)
	}
}

func TestReleaseComparisonMissingChangeAndConsistent(t *testing.T) {
	router := newTestRouter(t)
	seedComparisonStack(t, router)

	// dev 2.0.0 vs staging 2.0.0: alpha content differs, everything else equal.
	_, body := compareOK(t, router,
		"/api/v1/release-comparison?left=staging&left_version=2.1.0&right=dev&right_version=2.0.0")
	changes := body["changes"].([]any)
	if len(changes) != 2 {
		t.Fatalf("want alpha(changed) and gamma(missing): %v", changes)
	}
	if changes[1].(map[string]any)["change_id"] != "gamma" ||
		changes[1].(map[string]any)["comparison"] != "missing" {
		t.Fatalf("gamma must be missing in right->left direction: %v", changes[1])
	}

	// Identical releases (dev 1.0.0 vs staging 1.0.0, same content and facts)
	// produce an empty change list and a consistent verdict.
	_, body = compareOK(t, router,
		"/api/v1/release-comparison?left=dev&left_version=1.0.0&right=staging&right_version=1.0.0")
	if len(body["changes"].([]any)) != 0 {
		t.Fatalf("identical content must have empty changes: %v", body["changes"])
	}
	if body["consistent"] != true {
		t.Fatalf("identical releases must be consistent: %v", body)
	}
	totals := body["change_summary"].(map[string]any)
	if totals["added"] != float64(0) || totals["missing"] != float64(0) || totals["changed"] != float64(0) {
		t.Fatalf("totals must be zero: %v", totals)
	}
	rollback := body["rollback_point"].(map[string]any)
	if rollback["changed"] != false {
		t.Fatalf("same identifier + equal target objects must be consistent: %v", rollback)
	}
}

func TestReleaseComparisonRollbackTargetObjectMismatch(t *testing.T) {
	router := newTestRouter(t)
	seedComparisonStack(t, router)
	// prod 1.0.0 points at 1.0.0 (itself, beta+gamma blocked); staging 2.0.0
	// points at 1.0.0 (base allowed). Same rollback identifier, different
	// pointed release objects must be reported.
	_, body := compareOK(t, router,
		"/api/v1/release-comparison?left=staging&left_version=2.0.0&right=prod&right_version=1.0.0")
	rollback := body["rollback_point"].(map[string]any)
	if rollback["changed"] != true || rollback["target_changed"] != true {
		t.Fatalf("identical identifier with different target objects must differ: %v", rollback)
	}
	leftTarget := rollback["left_target"].(map[string]any)
	rightTarget := rollback["right_target"].(map[string]any)
	if leftTarget["version"] != "1.0.0" || rightTarget["version"] != "1.0.0" {
		t.Fatalf("target versions both 1.0.0: %v %v", leftTarget, rightTarget)
	}
	if leftTarget["environment"] != "staging" || rightTarget["environment"] != "prod" {
		t.Fatalf("targets keep their own environments: %v %v", leftTarget, rightTarget)
	}
}

func TestReleaseComparisonIsRepeatable(t *testing.T) {
	router := newTestRouter(t)
	seedComparisonStack(t, router)
	target := "/api/v1/release-comparison?left=dev&left_version=2.0.0&right=staging&right_version=2.1.0"
	first := doRequest(t, router, http.MethodGet, target, "")
	second := doRequest(t, router, http.MethodGet, target, "")
	if first.Body.String() != second.Body.String() {
		t.Fatalf("repeated comparisons must be byte-identical:\n%s\n%s", first.Body.String(), second.Body.String())
	}
}

func TestReleaseComparisonExactMatchingErrors(t *testing.T) {
	router := newTestRouter(t)
	seedComparisonStack(t, router)
	cases := []struct {
		name, target, code string
		status             int
	}{
		{"missing params",
			"/api/v1/release-comparison?left=dev&right=staging",
			"INVALID_RELEASE_COMPARISON_QUERY", http.StatusBadRequest},
		{"unknown left env",
			"/api/v1/release-comparison?left=nope&left_version=1.0.0&right=staging&right_version=1.0.0",
			"ReleaseComparisonEnvironmentNotFound", http.StatusNotFound},
		{"unknown right env",
			"/api/v1/release-comparison?left=dev&left_version=1.0.0&right=nope&right_version=1.0.0",
			"ReleaseComparisonEnvironmentNotFound", http.StatusNotFound},
		{"unknown version",
			"/api/v1/release-comparison?left=dev&left_version=9.9.9&right=staging&right_version=1.0.0",
			"ReleaseComparisonVersionNotFound", http.StatusNotFound},
		{"version is exact match, not fuzzy",
			"/api/v1/release-comparison?left=dev&left_version=2.0&right=staging&right_version=2.0.0",
			"ReleaseComparisonVersionNotFound", http.StatusNotFound},
		{"surrounding whitespace must not match stored value",
			"/api/v1/release-comparison?left=dev&left_version=+2.0.0&right=staging&right_version=2.0.0",
			"ReleaseComparisonVersionNotFound", http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := doRequest(t, router, http.MethodGet, tc.target, "")
			wantError(t, recorder, tc.status, tc.code)
		})
	}
}

func TestReleaseComparisonDataIncompleteWhenRollbackTargetMissing(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "qa")
	registerEnvironmentOK(t, router, "lab")
	// Both releases point at rollback identifiers that have no recorded release.
	createRecordOK(t, router, `{
	  "environment": "qa", "version": "3.0.0",
	  "changes": [], "gate_status": "pending", "rollback_point": "snapshot:missing@sha256:zz"}`)
	createRecordOK(t, router, `{
	  "environment": "lab", "version": "3.0.0",
	  "changes": [], "gate_status": "pending", "rollback_point": "snapshot:missing@sha256:zz"}`)
	recorder := doRequest(t, router, http.MethodGet,
		"/api/v1/release-comparison?left=qa&left_version=3.0.0&right=lab&right_version=3.0.0", "")
	wantError(t, recorder, http.StatusUnprocessableEntity, "ReleaseComparisonDataIncomplete")
}

func historyOK(t *testing.T, router *gin.Engine, target string) map[string]any {
	t.Helper()
	recorder := doRequest(t, router, http.MethodGet, target, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("history status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	return decodeBody(t, recorder)
}

func historyIDs(body map[string]any) []any {
	releases := body["releases"].([]any)
	ids := make([]any, 0, len(releases))
	for _, item := range releases {
		ids = append(ids, item.(map[string]any)["id"])
	}
	return ids
}

func TestReleaseHistoryNewestFirstAndPagination(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "prod")
	const total = 5
	createdIDs := make([]string, 0, total)
	for i := 0; i < total; i++ {
		body := "{\n" +
			"  \"environment\": \"prod\",\n" +
			"  \"version\": \"1.0." + string(rune('0'+i)) + "\",\n" +
			"  \"changes\": [\n" +
			"{\"sequence\": 1, \"category\": \"feature\", \"title\": \"c" + string(rune('0'+i)) + "\", \"description\": \"d\"}," +
			"{\"sequence\": 2, \"category\": \"fix\", \"title\": \"e" + string(rune('0'+i)) + "\", \"description\": \"d\"}," +
			"{\"sequence\": 3, \"category\": \"feature\", \"title\": \"f" + string(rune('0'+i)) + "\", \"description\": \"d\"}],\n" +
			"  \"gate_status\": \"allowed\", \"rollback_point\": \"1.0.0\"}"
		record := createRecordOK(t, router, body)
		createdIDs = append(createdIDs, record["id"].(string))
	}
	// Unresolvable rollback points never block history: it only reads facts.
	// First page: limit 2, newest first.
	page1 := historyOK(t, router, "/api/v1/environments/prod/release-history?limit=2")
	if page1["environment"] != "prod" || page1["limit"] != float64(2) {
		t.Fatalf("unexpected page1 header: %v", page1)
	}
	ids1 := historyIDs(page1)
	want1 := []string{createdIDs[4], createdIDs[3]}
	if len(ids1) != 2 || ids1[0] != want1[0] || ids1[1] != want1[1] {
		t.Fatalf("page1 ids = %v, want %v", ids1, want1)
	}
	cursor1 := page1["next_cursor"].(string)
	if cursor1 == "" {
		t.Fatal("expected a next cursor on a non-final page")
	}
	page2 := historyOK(t, router, "/api/v1/environments/prod/release-history?limit=2&cursor="+cursor1)
	ids2 := historyIDs(page2)
	want2 := []string{createdIDs[2], createdIDs[1]}
	if len(ids2) != 2 || ids2[0] != want2[0] || ids2[1] != want2[1] {
		t.Fatalf("page2 ids = %v, want %v", ids2, want2)
	}
	page3 := historyOK(t, router, "/api/v1/environments/prod/release-history?limit=2&cursor="+page2["next_cursor"].(string))
	ids3 := historyIDs(page3)
	if len(ids3) != 1 || ids3[0] != createdIDs[0] {
		t.Fatalf("page3 ids = %v, want only %s", ids3, createdIDs[0])
	}
	if page3["next_cursor"] != "" {
		t.Fatalf("final page must not carry a cursor: %v", page3["next_cursor"])
	}
	// No duplicates or omissions across pages.
	seen := map[string]int{}
	for _, id := range append(append(ids1, ids2...), ids3...) {
		seen[id.(string)]++
	}
	if len(seen) != total {
		t.Fatalf("paged ids must cover each record exactly once: %v", seen)
	}
	for id, count := range seen {
		if count != 1 {
			t.Fatalf("record %s appeared %d times", id, count)
		}
	}
	// Each entry traces back to the full recorded facts.
	first := page1["releases"].([]any)[0].(map[string]any)
	if first["version"] != "1.0.4" || first["gate_status"] != "allowed" || first["rollback_point"] != "1.0.0" {
		t.Fatalf("newest record must retain full facts: %v", first)
	}
	if len(first["changes"].([]any)) != 3 || first["recorded_at"] == "" {
		t.Fatalf("newest record must retain all three entries and time: %v", first)
	}
}

func TestReleaseHistoryDefaultLimitAndEmpty(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "empty")
	body := historyOK(t, router, "/api/v1/environments/empty/release-history")
	if len(body["releases"].([]any)) != 0 {
		t.Fatalf("fresh environment must have an empty releases array: %v", body)
	}
	if body["next_cursor"] != "" || body["limit"] != float64(defaultHistoryLimit) {
		t.Fatalf("empty page metadata wrong: %v", body)
	}
}

func TestReleaseHistoryErrors(t *testing.T) {
	router := newTestRouter(t)
	recorder := doRequest(t, router, http.MethodGet, "/api/v1/environments/ghost/release-history", "")
	wantError(t, recorder, http.StatusNotFound, "ReleaseComparisonEnvironmentNotFound")

	for _, target := range []string{
		"/api/v1/environments/ghost/release-history?limit=0",
		"/api/v1/environments/ghost/release-history?limit=abc",
		"/api/v1/environments/ghost/release-history?limit=99999",
		"/api/v1/environments/ghost/release-history?cursor=not-base64%21",
		"/api/v1/environments/ghost/release-history?cursor=eyJpZCI6MX0",
	} {
		recorder := doRequest(t, router, http.MethodGet, target, "")
		if recorder.Code == http.StatusNotFound &&
			decodeBody(t, recorder)["error"].(map[string]any)["code"] == "ReleaseComparisonEnvironmentNotFound" {
			t.Fatalf("pagination validation must run before environment lookup for %q: %s", target, recorder.Body.String())
		}
		wantError(t, recorder, http.StatusBadRequest, "INVALID_HISTORY_PAGINATION")
	}
}
