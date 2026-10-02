package v1

import (
	"net/http"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/luwa07832/deployment-tracker/internal/store"
)

func gateEvaluationPath(id string) string {
	return "/api/v1/release-records/" + id + "/gate-evaluations"
}

func setupGateRecord(t *testing.T, router *gin.Engine, environment, version, gate string) string {
	t.Helper()
	body := `{"environment":"` + environment + `","version":"` + version + `","changes":[],
		"gate_status":"` + gate + `","rollback_point":"snapshot:base"}`
	return createRecordOK(t, router, body)["id"].(string)
}

const passedCanaryChecks = `{"checks":[
	{"check_name":"canary","status":"passed","evidence":"build:42"}]}`

func TestCreateGateEvaluationSuccessAndIdempotency(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "prod")
	id := setupGateRecord(t, router, "prod", "1.2.0", "allowed")

	body := `{"checks":[
		{"check_name":"deploy-test","status":"passed","evidence":"job:7"},
		{"check_name":"canary","status":"waived","evidence":"ticket:3","waiver_reason":"known flake"}]}`
	recorder := doRequest(t, router, http.MethodPost, gateEvaluationPath(id), body)
	if recorder.Code != http.StatusOK {
		t.Fatalf("create status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	snapshot := decodeBody(t, recorder)["gate_evaluation"].(map[string]any)
	if snapshot["release"] != id {
		t.Fatalf("release = %v, want %s", snapshot["release"], id)
	}
	if snapshot["environment"] != "prod" || snapshot["version"] != "1.2.0" {
		t.Fatalf("location = %v", snapshot)
	}
	if snapshot["gate_status"] != "allowed" || snapshot["effective_gate_status"] != "allowed" {
		t.Fatalf("gate statuses = %v", snapshot)
	}
	checks := snapshot["checks"].([]any)
	if len(checks) != 2 {
		t.Fatalf("checks = %v", checks)
	}
	first := checks[0].(map[string]any)
	if first["check_name"] != "canary" || first["status"] != "waived" ||
		first["waiver_reason"] != "known flake" {
		t.Fatalf("checks not sorted by check_name: %v", checks)
	}
	second := checks[1].(map[string]any)
	if _, present := second["waiver_reason"]; present {
		t.Fatalf("waiver_reason must be omitted for non-waived checks: %v", second)
	}

	// Identical content, different submission order: same snapshot, still 200.
	reordered := `{"checks":[
		{"check_name":"canary","status":"waived","evidence":"ticket:3","waiver_reason":"known flake"},
		{"check_name":"deploy-test","status":"passed","evidence":"job:7"}]}`
	again := doRequest(t, router, http.MethodPost, gateEvaluationPath(id), reordered)
	if again.Code != http.StatusOK {
		t.Fatalf("resubmit status = %d, want 200 (body %s)", again.Code, again.Body.String())
	}
	againSnapshot := decodeBody(t, again)["gate_evaluation"].(map[string]any)
	if againSnapshot["checks"].([]any)[0].(map[string]any)["check_name"] != "canary" {
		t.Fatalf("resubmit snapshot changed order: %v", againSnapshot)
	}
}

func TestCreateGateEvaluationDerivesStatuses(t *testing.T) {
	cases := []struct {
		name       string
		recordGate string
		body       string
	}{
		{"failed", "blocked", `{"checks":[
			{"check_name":"a","status":"failed","evidence":"log"},
			{"check_name":"b","status":"pending","evidence":"log"}]}`},
		{"pending", "pending", `{"checks":[
			{"check_name":"a","status":"pending","evidence":"log"},
			{"check_name":"b","status":"passed","evidence":"log"}]}`},
		{"allowed", "allowed", `{"checks":[
			{"check_name":"a","status":"passed","evidence":"log"},
			{"check_name":"b","status":"waived","evidence":"log","waiver_reason":"ok"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router := newTestRouter(t)
			registerEnvironmentOK(t, router, "prod")
			id := setupGateRecord(t, router, "prod", "v-"+tc.name, tc.recordGate)
			recorder := doRequest(t, router, http.MethodPost, gateEvaluationPath(id), tc.body)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
			}
			snapshot := decodeBody(t, recorder)["gate_evaluation"].(map[string]any)
			if snapshot["gate_status"] != tc.recordGate ||
				snapshot["effective_gate_status"] != tc.recordGate {
				t.Fatalf("gate statuses = %v, want %s", snapshot, tc.recordGate)
			}
		})
	}
}

func TestCreateGateEvaluationFailures(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "prod")
	allowedID := setupGateRecord(t, router, "prod", "1.0.0", "allowed")
	blockedID := setupGateRecord(t, router, "prod", "1.0.1", "blocked")

	wantError(t, doRequest(t, router, http.MethodPost,
		gateEvaluationPath("rel_missing"), passedCanaryChecks),
		http.StatusNotFound, store.CodeReleaseRecordNotFoundV1)
	wantError(t, doRequest(t, router, http.MethodPost,
		gateEvaluationPath(allowedID), "not json"),
		http.StatusBadRequest, store.CodeInvalidRequest)

	validationBodies := []string{
		`{}`,
		`{"checks":[]}`,
		`{"checks":[{"status":"passed","evidence":"log"}]}`,
		`{"checks":[{"check_name":"  ","status":"passed","evidence":"log"}]}`,
		`{"checks":[{"check_name":"a","status":"passed","evidence":"log"},
			{"check_name":"a","status":"passed","evidence":"log2"}]}`,
		`{"checks":[{"check_name":"a","status":"approved","evidence":"log"}]}`,
		`{"checks":[{"check_name":"a","status":"passed","evidence":"  "}]}`,
		`{"checks":[{"check_name":"a","status":"passed","evidence":""}]}`,
		`{"checks":[{"check_name":"a","status":"waived","evidence":"log"}]}`,
		`{"checks":[{"check_name":"a","status":"passed","evidence":"log","waiver_reason":"why"}]}`,
		`{"checks":[{"check_name":"a","status":"passed","evidence":"log","waiver_reason":"   "}]}`,
	}
	for _, body := range validationBodies {
		recorder := doRequest(t, router, http.MethodPost, gateEvaluationPath(allowedID), body)
		wantError(t, recorder, http.StatusUnprocessableEntity, store.CodeGateEvaluationValidationV1)
	}

	// Derived status conflicts with the stored release gate_status.
	pendingBody := `{"checks":[{"check_name":"a","status":"pending","evidence":"log"}]}`
	wantError(t, doRequest(t, router, http.MethodPost, gateEvaluationPath(blockedID), pendingBody),
		http.StatusConflict, store.CodeGateStatusMismatchV1)

	// Establish a blocked snapshot, then submit different blocked-deriving content.
	failedBody := `{"checks":[{"check_name":"a","status":"failed","evidence":"log:1"}]}`
	if recorder := doRequest(t, router, http.MethodPost, gateEvaluationPath(blockedID), failedBody); recorder.Code != 200 {
		t.Fatalf("seed status = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	differentBlocked := `{"checks":[{"check_name":"a","status":"failed","evidence":"log:2"}]}`
	wantError(t, doRequest(t, router, http.MethodPost, gateEvaluationPath(blockedID), differentBlocked),
		http.StatusConflict, store.CodeGateEvaluationConflictV1)
}

func TestGetGateEvaluation(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "prod")
	id := setupGateRecord(t, router, "prod", "1.2.0", "allowed")

	wantError(t, doRequest(t, router, http.MethodGet, gateEvaluationPath(id), ""),
		http.StatusNotFound, store.CodeGateEvaluationNotFoundV1)
	wantError(t, doRequest(t, router, http.MethodGet, gateEvaluationPath("rel_missing"), ""),
		http.StatusNotFound, store.CodeGateEvaluationNotFoundV1)

	if recorder := doRequest(t, router, http.MethodPost, gateEvaluationPath(id), passedCanaryChecks); recorder.Code != 200 {
		t.Fatalf("seed: %s", recorder.Body.String())
	}
	recorder := doRequest(t, router, http.MethodGet, gateEvaluationPath(id), "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("get status = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	snapshot := decodeBody(t, recorder)["gate_evaluation"].(map[string]any)
	if snapshot["release"] != id || snapshot["gate_status"] != "allowed" {
		t.Fatalf("snapshot = %v", snapshot)
	}
	checks := snapshot["checks"].([]any)
	if len(checks) != 1 || checks[0].(map[string]any)["check_name"] != "canary" {
		t.Fatalf("checks = %v", checks)
	}
}

func seedGateSnapshot(t *testing.T, router *gin.Engine, environment, version, gate, body string) string {
	t.Helper()
	id := setupGateRecord(t, router, environment, version, gate)
	recorder := doRequest(t, router, http.MethodPost, gateEvaluationPath(id), body)
	if recorder.Code != http.StatusOK {
		t.Fatalf("seed %s %s: status %d body %s", environment, version, recorder.Code, recorder.Body.String())
	}
	return id
}

func TestListGateEvaluationsEmptyAndOrder(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "prod")

	empty := doRequest(t, router, http.MethodGet, "/api/v1/gate-evaluations", "")
	if empty.Code != http.StatusOK {
		t.Fatalf("empty list status = %d", empty.Code)
	}
	body := decodeBody(t, empty)
	if items, _ := body["gate_evaluations"].([]any); len(items) != 0 {
		t.Fatalf("empty list must be a definite []: %v", body)
	}
	if body["next_cursor"] != "" || len(body) != 2 {
		t.Fatalf("empty list body = %v", body)
	}

	blockedBody := `{"checks":[{"check_name":"z-last","status":"failed","evidence":"log"}]}`
	pendingBody := `{"checks":[{"check_name":"a-first","status":"pending","evidence":"log"}]}`
	allowedBody := `{"checks":[{"check_name":"m-middle","status":"passed","evidence":"log"}]}`
	seedGateSnapshot(t, router, "prod", "1.0.0", "allowed", allowedBody)
	seedGateSnapshot(t, router, "prod", "1.1.0", "pending", pendingBody)
	seedGateSnapshot(t, router, "prod", "1.2.0", "blocked", blockedBody)

	recorder := doRequest(t, router, http.MethodGet, "/api/v1/gate-evaluations", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("list status = %d", recorder.Code)
	}
	items := decodeBody(t, recorder)["gate_evaluations"].([]any)
	if len(items) != 3 {
		t.Fatalf("items = %v", items)
	}
	wantVersions := []string{"1.2.0", "1.1.0", "1.0.0"}
	for i, want := range wantVersions {
		if items[i].(map[string]any)["version"] != want {
			t.Fatalf("order[%d] = %v, want %s", i, items[i], want)
		}
	}
}

func TestListGateEvaluationsFilters(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "prod")
	registerEnvironmentOK(t, router, "staging")
	seedGateSnapshot(t, router, "prod", "1.0.0", "allowed",
		`{"checks":[{"check_name":"p","status":"passed","evidence":"log"}]}`)
	seedGateSnapshot(t, router, "prod", "1.1.0", "blocked",
		`{"checks":[{"check_name":"f","status":"failed","evidence":"log"},
			{"check_name":"p","status":"passed","evidence":"log"}]}`)
	seedGateSnapshot(t, router, "staging", "1.1.0", "pending",
		`{"checks":[{"check_name":"w","status":"waived","evidence":"log","waiver_reason":"ok"},
			{"check_name":"d","status":"pending","evidence":"log"}]}`)

	cases := []struct {
		query string
		want  int
	}{
		{"?environment=prod", 2},
		{"?environment=prod&gate_status=blocked", 1},
		{"?check_status=failed", 1},
		{"?check_status=pending", 1},
		{"?check_status=waived", 1},
		{"?check_status=passed", 2},
		{"?version=1.1.0", 2},
		{"?environment=staging&check_status=failed", 0},
	}
	for _, tc := range cases {
		recorder := doRequest(t, router, http.MethodGet, "/api/v1/gate-evaluations"+tc.query, "")
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s status = %d body %s", tc.query, recorder.Code, recorder.Body.String())
		}
		items := decodeBody(t, recorder)["gate_evaluations"].([]any)
		if len(items) != tc.want {
			t.Fatalf("%s items = %d, want %d (%s)", tc.query, len(items), tc.want, recorder.Body.String())
		}
	}

	// Filters apply to snapshot content and release location.
	recorder := doRequest(t, router, http.MethodGet,
		"/api/v1/gate-evaluations?environment=prod&check_status=failed", "")
	items := decodeBody(t, recorder)["gate_evaluations"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["version"] != "1.1.0" {
		t.Fatalf("failed-in-prod = %v", items)
	}
}

func TestListGateEvaluationsPagination(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "prod")
	for _, version := range []string{"1.0.0", "1.1.0", "1.2.0"} {
		seedGateSnapshot(t, router, "prod", version, "allowed",
			`{"checks":[{"check_name":"c","status":"passed","evidence":"log"}]}`)
	}

	first := doRequest(t, router, http.MethodGet, "/api/v1/gate-evaluations?limit=2", "")
	if first.Code != http.StatusOK {
		t.Fatalf("page 1: %s", first.Body.String())
	}
	firstBody := decodeBody(t, first)
	page1 := firstBody["gate_evaluations"].([]any)
	if len(page1) != 2 {
		t.Fatalf("page 1 size = %d", len(page1))
	}
	cursor, _ := firstBody["next_cursor"].(string)
	if cursor == "" {
		t.Fatalf("next_cursor missing: %v", firstBody)
	}

	second := doRequest(t, router, http.MethodGet, "/api/v1/gate-evaluations?limit=2&cursor="+cursor, "")
	if second.Code != http.StatusOK {
		t.Fatalf("page 2: %s", second.Body.String())
	}
	secondBody := decodeBody(t, second)
	page2 := secondBody["gate_evaluations"].([]any)
	if len(page2) != 1 || page2[0].(map[string]any)["version"] != "1.0.0" {
		t.Fatalf("page 2 = %v", page2)
	}
	if secondBody["next_cursor"] != "" {
		t.Fatalf("last page next_cursor = %v", secondBody["next_cursor"])
	}

	// Cursor is bound to the filter snapshot.
	rebound := doRequest(t, router, http.MethodGet,
		"/api/v1/gate-evaluations?limit=2&gate_status=blocked&cursor="+cursor, "")
	wantError(t, rebound, http.StatusBadRequest, store.CodeInvalidGateEvaluationQueryV1)
	truncated := doRequest(t, router, http.MethodGet,
		"/api/v1/gate-evaluations?cursor="+cursor[:len(cursor)-2], "")
	wantError(t, truncated, http.StatusBadRequest, store.CodeInvalidGateEvaluationQueryV1)
}

func TestListGateEvaluationsQueryFailures(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "prod")

	invalidQueries := []string{
		"?limit=0",
		"?limit=101",
		"?limit=abc",
		"?limit=",
		"?cursor=",
		"?cursor=garbage",
		"?gate_status=unknown",
		"?check_status=approved",
		"?environment=",
		"?version=",
	}
	for _, query := range invalidQueries {
		recorder := doRequest(t, router, http.MethodGet, "/api/v1/gate-evaluations"+query, "")
		wantError(t, recorder, http.StatusBadRequest, store.CodeInvalidGateEvaluationQueryV1)
	}

	wantError(t, doRequest(t, router, http.MethodGet,
		"/api/v1/gate-evaluations?environment=unknown", ""),
		http.StatusNotFound, store.CodeEnvironmentNotFoundV1)
}

func TestGateEvaluationStorageFailure(t *testing.T) {
	router, db := newTestRouterWithStore(t)
	registerEnvironmentOK(t, router, "prod")
	id := setupGateRecord(t, router, "prod", "1.0.0", "allowed")
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	wantError(t, doRequest(t, router, http.MethodPost, gateEvaluationPath(id), passedCanaryChecks),
		http.StatusServiceUnavailable, store.CodeStorageUnavailable)
	wantError(t, doRequest(t, router, http.MethodGet, gateEvaluationPath(id), ""),
		http.StatusServiceUnavailable, store.CodeStorageUnavailable)
	wantError(t, doRequest(t, router, http.MethodGet, "/api/v1/gate-evaluations", ""),
		http.StatusServiceUnavailable, store.CodeStorageUnavailable)
}

func TestConcurrentIdenticalGateEvaluationsLeaveOneSnapshot(t *testing.T) {
	router, db := newTestRouterWithStore(t)
	registerEnvironmentOK(t, router, "prod")
	id := setupGateRecord(t, router, "prod", "1.0.0", "allowed")

	const total = 8
	statuses := make([]int, total)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			recorder := doRequest(t, router, http.MethodPost, gateEvaluationPath(id), passedCanaryChecks)
			statuses[i] = recorder.Code
		}(i)
	}
	close(start)
	wg.Wait()

	for i, status := range statuses {
		if status != http.StatusOK {
			t.Fatalf("request %d status = %d, want 200", i, status)
		}
	}
	var count int
	if err := db.CountGateEvaluationsForTest(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("snapshot count = %d, want 1", count)
	}
}
