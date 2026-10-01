package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/luwa07832/deployment-tracker/internal/store"
)

func newReleaseTestRouter(t *testing.T) (*gin.Engine, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewRouter(Dependencies{Store: db}), path
}

func doRequest(t *testing.T, router *gin.Engine, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, target, reader)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func decodeBody(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body %q: %v", recorder.Body.String(), err)
	}
	return body
}

func wantError(t *testing.T, recorder *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if recorder.Code != status {
		t.Fatalf("status = %d, want %d (body %s)", recorder.Code, status, recorder.Body.String())
	}
	body := decodeBody(t, recorder)
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("body %s has no error object", recorder.Body.String())
	}
	if errObj["code"] != code {
		t.Fatalf("error.code = %v, want %s", errObj["code"], code)
	}
	if msg, _ := errObj["message"].(string); msg == "" {
		t.Fatalf("error.message is empty in %s", recorder.Body.String())
	}
	if len(body) != 1 {
		t.Fatalf("error body %s must contain only the error key", recorder.Body.String())
	}
}

func createReleaseOK(t *testing.T, router *gin.Engine, body string) map[string]any {
	t.Helper()
	recorder := doRequest(t, router, http.MethodPost, "/releases", body)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201 (body %s)", recorder.Code, recorder.Body.String())
	}
	return decodeBody(t, recorder)["release"].(map[string]any)
}

func TestCreateReleaseReturnsTheStoredRecord(t *testing.T) {
	router, _ := newReleaseTestRouter(t)
	release := createReleaseOK(t, router, `{
		"environment": "prod",
		"version": "1.2.0",
		"changes": ["add login", "fix logout"],
		"gate_status": "allowed",
		"rollback_point": "1.1.0"
	}`)
	if release["environment"] != "prod" || release["version"] != "1.2.0" {
		t.Fatalf("release = %v", release)
	}
	if release["gate_status"] != "allowed" || release["rollback_point"] != "1.1.0" {
		t.Fatalf("release = %v", release)
	}
	changes, ok := release["changes"].([]any)
	if !ok || len(changes) != 2 || changes[0] != "add login" || changes[1] != "fix logout" {
		t.Fatalf("changes = %v", release["changes"])
	}
	if release["id"] == nil || release["registered_at"] == "" {
		t.Fatalf("release missing id/registered_at: %v", release)
	}
}

func TestCreateReleaseIdempotentAndConflict(t *testing.T) {
	router, _ := newReleaseTestRouter(t)
	first := createReleaseOK(t, router, `{"environment":"prod","version":"1.0.0","changes":["a","b"],"gate_status":"allowed","rollback_point":"0.9.0"}`)
	// Identical content returns the existing record, change order aside.
	recorder := doRequest(t, router, http.MethodPost, "/releases", `{"environment":"prod","version":"1.0.0","changes":["b","a"],"gate_status":"allowed","rollback_point":"0.9.0"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("re-register status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	again := decodeBody(t, recorder)["release"].(map[string]any)
	if again["id"] != first["id"] {
		t.Fatalf("re-register id = %v, want existing %v", again["id"], first["id"])
	}
	// Different content conflicts.
	for _, body := range []string{
		`{"environment":"prod","version":"1.0.0","changes":["a","b","c"],"gate_status":"allowed","rollback_point":"0.9.0"}`,
		`{"environment":"prod","version":"1.0.0","changes":["a","b"],"gate_status":"blocked","rollback_point":"0.9.0"}`,
		`{"environment":"prod","version":"1.0.0","changes":["a","b"],"gate_status":"allowed","rollback_point":"0.8.0"}`,
	} {
		wantError(t, doRequest(t, router, http.MethodPost, "/releases", body), http.StatusConflict, store.CodeReleaseConflict)
	}
}

func TestCreateReleaseValidation(t *testing.T) {
	router, _ := newReleaseTestRouter(t)
	invalid := []string{
		`{"version":"1.0.0","changes":["a"],"gate_status":"allowed","rollback_point":"0.9.0"}`,
		`{"environment":"prod","changes":["a"],"gate_status":"allowed","rollback_point":"0.9.0"}`,
		`{"environment":"prod","version":"1.0.0","gate_status":"allowed","rollback_point":"0.9.0"}`,
		`{"environment":"prod","version":"1.0.0","changes":["a"],"rollback_point":"0.9.0"}`,
		`{"environment":"prod","version":"1.0.0","changes":["a"],"gate_status":"allowed"}`,
		`{"environment":" ","version":"1.0.0","changes":["a"],"gate_status":"allowed","rollback_point":"0.9.0"}`,
		`{"environment":"prod","version":"1.0.0","changes":["a"],"gate_status":"shipped","rollback_point":"0.9.0"}`,
		`{"environment":"prod","version":"1.0.0","changes":["a",""],"gate_status":"allowed","rollback_point":"0.9.0"}`,
		`{"environment":"prod","version":"1.0.0","changes":"a","gate_status":"allowed","rollback_point":"0.9.0"}`,
	}
	for _, body := range invalid {
		wantError(t, doRequest(t, router, http.MethodPost, "/releases", body), http.StatusBadRequest, store.CodeInvalidReleaseInput)
	}
	wantError(t, doRequest(t, router, http.MethodPost, "/releases", `{not json`), http.StatusBadRequest, store.CodeInvalidRequest)
}

func TestListReleasesFilters(t *testing.T) {
	router, _ := newReleaseTestRouter(t)
	createReleaseOK(t, router, `{"environment":"staging","version":"1.0.0","changes":["add login"],"gate_status":"allowed","rollback_point":"0.9.0"}`)
	createReleaseOK(t, router, `{"environment":"prod","version":"1.0.0","changes":["add login","drop table"],"gate_status":"blocked","rollback_point":"0.9.0"}`)
	createReleaseOK(t, router, `{"environment":"prod","version":"1.1.0","changes":["fix logout"],"gate_status":"allowed","rollback_point":"1.0.0"}`)
	cases := []struct {
		query string
		want  int
	}{
		{"/releases", 3},
		{"/releases?environment=prod", 2},
		{"/releases?version=1.0.0", 2},
		{"/releases?change=drop%20table", 1},
		{"/releases?gate_status=blocked", 1},
		{"/releases?rollback_point=1.0.0", 1},
		{"/releases?environment=prod&gate_status=allowed", 1},
		{"/releases?environment=prod&version=9.9.9", 0},
	}
	for _, tc := range cases {
		recorder := doRequest(t, router, http.MethodGet, tc.query, "")
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200 (body %s)", tc.query, recorder.Code, recorder.Body.String())
		}
		releases := decodeBody(t, recorder)["releases"].([]any)
		if len(releases) != tc.want {
			t.Fatalf("%s returned %d releases, want %d", tc.query, len(releases), tc.want)
		}
	}
	// Empty results stay empty collections, not null.
	recorder := doRequest(t, router, http.MethodGet, "/releases?version=9.9.9", "")
	if !strings.Contains(recorder.Body.String(), `"releases":[]`) {
		t.Fatalf("empty result body = %s, want an empty releases array", recorder.Body.String())
	}
	// Unknown environments are an error.
	wantError(t, doRequest(t, router, http.MethodGet, "/releases?environment=nowhere", ""), http.StatusNotFound, store.CodeEnvironmentNotFound)
}

func TestGetReleaseByEnvironmentAndVersion(t *testing.T) {
	router, _ := newReleaseTestRouter(t)
	created := createReleaseOK(t, router, `{"environment":"prod","version":"1.0.0","changes":["a"],"gate_status":"pending","rollback_point":"0.9.0"}`)
	recorder := doRequest(t, router, http.MethodGet, "/releases/prod/1.0.0", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	got := decodeBody(t, recorder)["release"].(map[string]any)
	if got["id"] != created["id"] || got["gate_status"] != "pending" {
		t.Fatalf("got = %v, want the created record", got)
	}
	wantError(t, doRequest(t, router, http.MethodGet, "/releases/nowhere/1.0.0", ""), http.StatusNotFound, store.CodeEnvironmentNotFound)
	wantError(t, doRequest(t, router, http.MethodGet, "/releases/prod/9.9.9", ""), http.StatusNotFound, store.CodeReleaseNotFound)
}

func TestHistoryReturnsRegistrationOrder(t *testing.T) {
	router, _ := newReleaseTestRouter(t)
	createReleaseOK(t, router, `{"environment":"prod","version":"1.2.0","changes":["c"],"gate_status":"pending","rollback_point":"1.1.0"}`)
	createReleaseOK(t, router, `{"environment":"prod","version":"1.0.0","changes":["a"],"gate_status":"allowed","rollback_point":"0.9.0"}`)
	createReleaseOK(t, router, `{"environment":"staging","version":"1.0.0","changes":["a"],"gate_status":"allowed","rollback_point":"0.9.0"}`)
	recorder := doRequest(t, router, http.MethodGet, "/environments/prod/history", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	body := decodeBody(t, recorder)
	if body["environment"] != "prod" {
		t.Fatalf("environment = %v, want prod", body["environment"])
	}
	releases := body["releases"].([]any)
	if len(releases) != 2 {
		t.Fatalf("history has %d records, want 2", len(releases))
	}
	first := releases[0].(map[string]any)
	second := releases[1].(map[string]any)
	if first["version"] != "1.2.0" || second["version"] != "1.0.0" {
		t.Fatalf("history order = %v then %v, want registration order 1.2.0 then 1.0.0", first["version"], second["version"])
	}
	if first["rollback_point"] != "1.1.0" || second["rollback_point"] != "0.9.0" {
		t.Fatalf("history lost rollback points: %v, %v", first, second)
	}
	wantError(t, doRequest(t, router, http.MethodGet, "/environments/nowhere/history", ""), http.StatusNotFound, store.CodeEnvironmentNotFound)
}

// TestLegacyRecordsStayReadableOverHTTP seeds a row without the new columns
// and checks it reads back with the new fields skipped, not rejected.
func TestLegacyRecordsStayReadableOverHTTP(t *testing.T) {
	router, path := newReleaseTestRouter(t)
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO deployments (name, environment, version) VALUES ('legacy', 'prod', '0.9.0')`); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}
	raw.Close()
	recorder := doRequest(t, router, http.MethodGet, "/releases/prod/0.9.0", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	release := decodeBody(t, recorder)["release"].(map[string]any)
	if release["environment"] != "prod" || release["version"] != "0.9.0" {
		t.Fatalf("legacy release = %v", release)
	}
	for _, skipped := range []string{"changes", "gate_status", "rollback_point"} {
		if _, present := release[skipped]; present {
			t.Fatalf("legacy release must skip %s, got %v", skipped, release)
		}
	}
	// History over a legacy-only environment works and skips the new fields.
	recorder = doRequest(t, router, http.MethodGet, "/environments/prod/history", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("history status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	releases := decodeBody(t, recorder)["releases"].([]any)
	if len(releases) != 1 {
		t.Fatalf("legacy history = %v, want 1 record", releases)
	}
}
