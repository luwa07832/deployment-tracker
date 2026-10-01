package v1

import (
	"net/http"
	"testing"

	"github.com/luwa07832/deployment-tracker/internal/store"
)

func TestRegisterEnvironmentLifecycle(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "prod")

	// Re-registering is idempotent and returns 200 with the stored row.
	recorder := doRequest(t, router, http.MethodPost, "/api/v1/environments",
		`{"environment":"prod","display_name":"Different"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("re-register status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	env := decodeBody(t, recorder)["environment"].(map[string]any)
	if env["display_name"] != "prod display" || env["registered_at"] == "" {
		t.Fatalf("idempotent register changed the row: %v", env)
	}

	recorder = doRequest(t, router, http.MethodGet, "/api/v1/environments", "")
	environments := decodeBody(t, recorder)["environments"].([]any)
	if len(environments) != 1 {
		t.Fatalf("environments = %v, want 1", environments)
	}

	recorder = doRequest(t, router, http.MethodGet, "/api/v1/environments/prod", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("get env status = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	wantError(t, doRequest(t, router, http.MethodGet, "/api/v1/environments/missing", ""),
		http.StatusNotFound, store.CodeEnvironmentNotFoundV1)

	wantError(t, doRequest(t, router, http.MethodPost, "/api/v1/environments", `{"environment":" "}`),
		http.StatusUnprocessableEntity, store.CodeEnvironmentValidationV1)
	wantError(t, doRequest(t, router, http.MethodPost, "/api/v1/environments", `{bad`),
		http.StatusBadRequest, store.CodeInvalidRequest)
}
