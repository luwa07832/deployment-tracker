package v1

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/luwa07832/deployment-tracker/internal/store"
)

func newTestRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := store.Open(filepath.Join(t.TempDir(), "v1.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	engine := gin.New()
	Register(engine, Dependencies{Store: db})
	return engine
}

func doRequest(t *testing.T, router *gin.Engine, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
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
	if !ok || errObj["code"] != code {
		t.Fatalf("body %s, want error code %s", recorder.Body.String(), code)
	}
	if msg, _ := errObj["message"].(string); msg == "" {
		t.Fatalf("error message empty in %s", recorder.Body.String())
	}
	if len(body) != 1 {
		t.Fatalf("error body must contain only the error key: %s", recorder.Body.String())
	}
}

func registerEnvironmentOK(t *testing.T, router *gin.Engine, key string) {
	t.Helper()
	body := `{"environment":"` + key + `","display_name":"` + key + " display\"}"
	recorder := doRequest(t, router, http.MethodPost, "/api/v1/environments", body)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("register %s status = %d, want 201 (body %s)", key, recorder.Code, recorder.Body.String())
	}
}

const validRecordBody = `{
  "environment": "prod",
  "version": "1.2.0",
  "changes": [
    {"sequence": 1, "category": "feature", "title": "add login", "description": "users can log in"},
    {"sequence": 2, "category": "fix", "title": "fix logout", "description": "logout clears the session"}
  ],
  "gate_status": "allowed",
  "rollback_point": "snapshot:1.1.0"
}`

func createRecordOK(t *testing.T, router *gin.Engine, body string) map[string]any {
	t.Helper()
	recorder := doRequest(t, router, http.MethodPost, "/api/v1/release-records", body)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201 (body %s)", recorder.Code, recorder.Body.String())
	}
	record := decodeBody(t, recorder)["release_record"].(map[string]any)
	return record
}
