package v1

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

const gateChecksBody = `{
  "checks": [
    {"check_name": "security-scan", "status": "passed", "evidence": "scan report #12"},
    {"check_name": "canary", "status": "waived", "evidence": "manual sign-off", "waiver_reason": "no canary traffic window"},
    {"check_name": "build", "status": "passed", "evidence": "ci run #88"}
  ]
}`

func gateURL(recordID string) string {
	return "/api/v1/release-records/" + recordID + "/gate-evaluations"
}

func createGateEvaluationOK(t *testing.T, router *gin.Engine, recordID, checks string) map[string]any {
	t.Helper()
	recorder := doRequest(t, router, http.MethodPost, gateURL(recordID),
		`{"checks":[`+checks+`]}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create gate evaluation status = %d, want 201 (body %s)",
			recorder.Code, recorder.Body.String())
	}
	return gateView(t, recorder)
}

func gateView(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	return decodeBody(t, recorder)["gate_evaluation"].(map[string]any)
}

func gateCheckNames(view map[string]any) []string {
	raw := view["checks"].([]any)
	names := make([]string, 0, len(raw))
	for _, item := range raw {
		names = append(names, item.(map[string]any)["check_name"].(string))
	}
	return names
}

func TestCreateGateEvaluationSuccess(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "prod")
	record := createRecordOK(t, router, validRecordBody)

	recorder := doRequest(t, router, http.MethodPost, gateURL(record["id"].(string)), gateChecksBody)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", recorder.Code, recorder.Body.String())
	}
	view := gateView(t, recorder)
	if view["release_record_id"] != record["id"] || view["environment"] != "prod" ||
		view["version"] != "1.2.0" {
		t.Fatalf("locator mismatch: %v", view)
	}
	if view["gate_status"] != "allowed" || view["effective_gate_status"] != "allowed" {
		t.Fatalf("statuses mismatch: %v", view)
	}
	if names := strings.Join(gateCheckNames(view), ","); names != "build,canary,security-scan" {
		t.Fatalf("checks not sorted by check_name: %s", names)
	}
	// batch_id must be omitted for an unbatched record.
	body := recorder.Body.String()
	if strings.Contains(body, "batch_id") {
		t.Fatalf("unbatched snapshot must omit batch_id: %s", body)
	}
	// Non-waived checks omit waiver_reason.
	if strings.Contains(body, `"waiver_reason":""`) {
		t.Fatalf("non-waived checks must omit waiver_reason: %s", body)
	}

	// Same content (reordered) returns 200 with the same snapshot.
	recorder2 := doRequest(t, router, http.MethodPost, gateURL(record["id"].(string)),
		`{"checks":[
		  {"check_name":"build","status":"passed","evidence":"ci run #88"},
		  {"check_name":"security-scan","status":"passed","evidence":"scan report #12"},
		  {"check_name":"canary","status":"waived","evidence":"manual sign-off","waiver_reason":"no canary traffic window"}
		]}`)
	if recorder2.Code != http.StatusOK {
		t.Fatalf("identical resubmit status = %d, want 200 (body %s)", recorder2.Code, recorder2.Body.String())
	}
	if gateView(t, recorder2)["release_record_id"] != record["id"] {
		t.Fatal("identical resubmit must return the same snapshot")
	}
}

func TestCreateGateEvaluationBatchedKeepsBatchID(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "prod")
	body := strings.Replace(validRecordBody, `"version": "1.2.0"`,
		`"version": "1.2.0","batch_id": "b-1"`, 1)
	record := createRecordOK(t, router, body)

	recorder := doRequest(t, router, http.MethodPost, gateURL(record["id"].(string)),
		`{"checks":[{"check_name":"build","status":"passed","evidence":"ci"}]}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", recorder.Code, recorder.Body.String())
	}
	if gateView(t, recorder)["batch_id"] != "b-1" {
		t.Fatalf("batched snapshot must expose batch_id: %s", recorder.Body.String())
	}
}

func TestCreateGateEvaluationErrors(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "prod")
	record := createRecordOK(t, router, validRecordBody)
	id := record["id"].(string)

	// Unknown release record.
	recorder := doRequest(t, router, http.MethodPost, gateURL("rel_missing"),
		`{"checks":[{"check_name":"a","status":"passed","evidence":"e"}]}`)
	wantError(t, recorder, http.StatusNotFound, "RELEASE_RECORD_NOT_FOUND")

	// Invalid JSON.
	recorder = doRequest(t, router, http.MethodPost, gateURL(id), `{not json`)
	wantError(t, recorder, http.StatusBadRequest, "invalid_request")

	cases := []struct {
		name string
		body string
	}{
		{"missing checks", `{}`},
		{"empty checks", `{"checks":[]}`},
		{"blank check_name", `{"checks":[{"check_name":"  ","status":"passed","evidence":"e"}]}`},
		{"duplicate check_name", `{"checks":[
		  {"check_name":"a","status":"passed","evidence":"e"},
		  {"check_name":" a ","status":"passed","evidence":"f"}]}`},
		{"invalid status", `{"checks":[{"check_name":"a","status":"nope","evidence":"e"}]}`},
		{"blank evidence", `{"checks":[{"check_name":"a","status":"passed","evidence":"  "}]}`},
		{"waived without reason", `{"checks":[{"check_name":"a","status":"waived","evidence":"e"}]}`},
		{"blank waiver reason", `{"checks":[{"check_name":"a","status":"waived","evidence":"e","waiver_reason":"  "}]}`},
		{"reason on passed", `{"checks":[{"check_name":"a","status":"passed","evidence":"e","waiver_reason":"why"}]}`},
		{"wrong field type", `{"checks":[{"check_name":"a","status":7,"evidence":"e"}]}`},
	}
	for _, tc := range cases {
		recorder := doRequest(t, router, http.MethodPost, gateURL(id), tc.body)
		if recorder.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d, want 422 (body %s)", tc.name, recorder.Code, recorder.Body.String())
			continue
		}
		wantError(t, recorder, http.StatusUnprocessableEntity, "GATE_EVALUATION_VALIDATION_FAILED")
	}
}

func TestCreateGateEvaluationDerivedStatuses(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "prod")

	counter := 0
	decision := func(gate, checkBody string) int {
		counter++
		record := createRecordOK(t, router, recordBody("prod", "v-"+strings.Repeat("x", counter), gate, "rb",
			entry(1, "feature", "x", "y")))
		recorder := doRequest(t, router, http.MethodPost, gateURL(record["id"].(string)), checkBody)
		return recorder.Code
	}
	// allowed record, pending check -> mismatch.
	bodyPending := `{"checks":[{"check_name":"a","status":"pending","evidence":"e"}]}`
	if code := decision("allowed", bodyPending); code != http.StatusConflict {
		t.Fatalf("allowed record + pending check = %d, want 409", code)
	}
	// blocked record, failed check -> created.
	bodyFailed := `{"checks":[{"check_name":"a","status":"failed","evidence":"e"}]}`
	if code := decision("blocked", bodyFailed); code != http.StatusCreated {
		t.Fatalf("blocked record + failed check = %d, want 201", code)
	}
	// pending record, pending check -> created.
	if code := decision("pending", bodyPending); code != http.StatusCreated {
		t.Fatalf("pending record + pending check = %d, want 201", code)
	}
	// allowed record, failed check -> mismatch 409 GATE_STATUS_MISMATCH.
	if code := decision("allowed", bodyFailed); code != http.StatusConflict {
		t.Fatalf("allowed record + failed check = %d, want 409", code)
	}
	// Verify mismatch code on an explicit request.
	record := createRecordOK(t, router, recordBody("prod", "v-mismatch", "allowed", "rb",
		entry(1, "feature", "x", "y")))
	recorder := doRequest(t, router, http.MethodPost, gateURL(record["id"].(string)), bodyFailed)
	wantError(t, recorder, http.StatusConflict, "GATE_STATUS_MISMATCH")
}

func TestCreateGateEvaluationConflict(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "prod")
	record := createRecordOK(t, router, validRecordBody)
	id := record["id"].(string)
	first := `{"checks":[{"check_name":"build","status":"passed","evidence":"ci #1"}]}`
	second := `{"checks":[{"check_name":"build","status":"passed","evidence":"ci #2"}]}`
	if r := doRequest(t, router, http.MethodPost, gateURL(id), first); r.Code != http.StatusCreated {
		t.Fatalf("first = %d, %s", r.Code, r.Body.String())
	}
	wantError(t, doRequest(t, router, http.MethodPost, gateURL(id), second),
		http.StatusConflict, "GATE_EVALUATION_CONFLICT")
}

func TestGetGateEvaluation(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "prod")
	record := createRecordOK(t, router, validRecordBody)
	id := record["id"].(string)

	// No snapshot yet -> 404 GATE_EVALUATION_NOT_FOUND.
	wantError(t, doRequest(t, router, http.MethodGet, gateURL(id), ""),
		http.StatusNotFound, "GATE_EVALUATION_NOT_FOUND")
	// Unknown record id uses the same not-found code.
	wantError(t, doRequest(t, router, http.MethodGet, gateURL("rel_missing"), ""),
		http.StatusNotFound, "GATE_EVALUATION_NOT_FOUND")

	createGateView := createGateEvaluationOK(t, router, id,
		`{"check_name":"build","status":"passed","evidence":"ci #1"},
		 {"check_name":"tests","status":"passed","evidence":"ci #2"}`)
	_ = createGateView

	recorder := doRequest(t, router, http.MethodGet, gateURL(id), "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("get status = %d, body %s", recorder.Code, recorder.Body.String())
	}
	view := gateView(t, recorder)
	if view["gate_status"] != "allowed" || view["effective_gate_status"] != "allowed" {
		t.Fatalf("statuses = %v", view)
	}
	if names := strings.Join(gateCheckNames(view), ","); names != "build,tests" {
		t.Fatalf("check order = %s", names)
	}
}

func seedGateRecord(t *testing.T, router *gin.Engine, environment, version, gate, checkStatus string) string {
	t.Helper()
	record := createRecordOK(t, router, recordBody(environment, version, gate, "rb",
		entry(1, "feature", "x", "y")))
	check := `{"check_name":"g","status":"` + checkStatus + `","evidence":"e"`
	if checkStatus == "waived" {
		check += `,"waiver_reason":"accepted"`
	}
	check += `}`
	createGateEvaluationOK(t, router, record["id"].(string), check)
	return record["id"].(string)
}

func TestListGateEvaluationsFiltersPaginationAndOrder(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "prod")
	registerEnvironmentOK(t, router, "staging")

	// Three snapshots, newest first expected: C (staging/pending), B (prod/blocked), A (prod/allowed).
	idA := seedGateRecord(t, router, "prod", "1.0.0", "allowed", "passed")
	idB := seedGateRecord(t, router, "prod", "1.1.0", "blocked", "failed")
	idC := seedGateRecord(t, router, "staging", "2.0.0", "pending", "pending")

	recorder := doRequest(t, router, http.MethodGet, "/api/v1/gate-evaluations", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("list = %d, %s", recorder.Code, recorder.Body.String())
	}
	body := decodeBody(t, recorder)
	if len(body) != 2 {
		t.Fatalf("list body must have gate_evaluations and next_cursor only: %s", recorder.Body.String())
	}
	items := body["gate_evaluations"].([]any)
	if len(items) != 3 {
		t.Fatalf("list len = %d, want 3", len(items))
	}
	if items[0].(map[string]any)["release_record_id"] != idC ||
		items[2].(map[string]any)["release_record_id"] != idA {
		t.Fatal("list must be newest first")
	}
	if body["next_cursor"] != "" {
		t.Fatalf("single page next_cursor must be empty: %v", body["next_cursor"])
	}

	// Empty result serializes as [].
	empty := doRequest(t, router, http.MethodGet,
		"/api/v1/gate-evaluations?environment=prod&version=9.9.9", "")
	if empty.Code != http.StatusOK || !strings.Contains(empty.Body.String(), `"gate_evaluations":[]`) {
		t.Fatalf("empty result wrong: %d %s", empty.Code, empty.Body.String())
	}

	// Filters.
	expectIDs := func(query string, want ...string) {
		t.Helper()
		r := doRequest(t, router, http.MethodGet, "/api/v1/gate-evaluations?"+query, "")
		if r.Code != http.StatusOK {
			t.Fatalf("%s status = %d, %s", query, r.Code, r.Body.String())
		}
		gotItems := decodeBody(t, r)["gate_evaluations"].([]any)
		if len(gotItems) != len(want) {
			t.Fatalf("%s len = %d, want %d (%s)", query, len(gotItems), len(want), r.Body.String())
		}
		for i, item := range gotItems {
			if item.(map[string]any)["release_record_id"] != want[i] {
				t.Fatalf("%s item %d = %v, want %s", query, i, item, want[i])
			}
		}
	}
	expectIDs("environment=prod", idB, idA)
	expectIDs("gate_status=blocked", idB)
	expectIDs("check_status=failed", idB)
	expectIDs("check_status=pending", idC)
	expectIDs("version=1.0.0", idA)
	expectIDs("environment=staging&gate_status=pending&check_status=pending", idC)

	// Unregistered environment -> 404.
	wantError(t, doRequest(t, router, http.MethodGet,
		"/api/v1/gate-evaluations?environment=ghost", ""),
		http.StatusNotFound, "ENVIRONMENT_NOT_FOUND")

	// Query-shape errors return 400 INVALID_GATE_EVALUATION_QUERY.
	invalidQueries := []string{
		"limit=",
		"limit=abc",
		"limit=0",
		"limit=101",
		"gate_status=nope",
		"check_status=nope",
		"environment=",
		"version=",
		"cursor=",
		"cursor=tampered",
	}
	for _, query := range invalidQueries {
		r := doRequest(t, router, http.MethodGet, "/api/v1/gate-evaluations?"+query, "")
		wantError(t, r, http.StatusBadRequest, "INVALID_GATE_EVALUATION_QUERY")
	}

	// Pagination walk with limit 1 visits all three once, newest first.
	path := "/api/v1/gate-evaluations?limit=1"
	var walked []string
	for page := 0; page < 5; page++ {
		r := doRequest(t, router, http.MethodGet, path, "")
		if r.Code != http.StatusOK {
			t.Fatalf("page %d = %d, %s", page, r.Code, r.Body.String())
		}
		pageBody := decodeBody(t, r)
		pageItems := pageBody["gate_evaluations"].([]any)
		if len(pageItems) != 1 {
			t.Fatalf("page %d len = %d", page, len(pageItems))
		}
		walked = append(walked, pageItems[0].(map[string]any)["release_record_id"].(string))
		next, _ := pageBody["next_cursor"].(string)
		if next == "" {
			break
		}
		path = "/api/v1/gate-evaluations?limit=1&cursor=" + next
	}
	want := []string{idC, idB, idA}
	if strings.Join(walked, ",") != strings.Join(want, ",") {
		t.Fatalf("walk = %v, want %v", walked, want)
	}
}
