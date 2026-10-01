package v1

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func registerRouteOK(t *testing.T, router *gin.Engine, name string, environments []string) map[string]any {
	t.Helper()
	body := fmt.Sprintf(`{"name":%q,"environments":[`, name)
	for i, env := range environments {
		if i > 0 {
			body += ","
		}
		body += fmt.Sprintf("%q", env)
	}
	body += "]}"
	recorder := doRequest(t, router, http.MethodPost, "/api/v1/promotion-routes", body)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create route status = %d, want 201 (body %s)", recorder.Code, recorder.Body.String())
	}
	return decodeBody(t, recorder)["promotion_route"].(map[string]any)
}

func TestPromotionRouteCreateGetList(t *testing.T) {
	router := newTestRouter(t)
	for _, env := range []string{"dev", "test", "prod"} {
		registerEnvironmentOK(t, router, env)
	}

	// Create 201; name is trimmed, environments stay in order.
	route := registerRouteOK(t, router, "  prod-line  ", []string{"dev", "test", "prod"})
	if route["name"] != "prod-line" {
		t.Fatalf("name = %v, want trimmed prod-line", route["name"])
	}
	envs := route["environments"].([]any)
	if len(envs) != 3 || envs[0] != "dev" || envs[1] != "test" || envs[2] != "prod" {
		t.Fatalf("environments = %v", envs)
	}

	// Same sequence is idempotent: 200 with the same payload.
	recorder := doRequest(t, router, http.MethodPost, "/api/v1/promotion-routes",
		`{"name":"prod-line","environments":["dev","test","prod"]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("same sequence status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}

	// Different sequence conflicts.
	recorder = doRequest(t, router, http.MethodPost, "/api/v1/promotion-routes",
		`{"name":"prod-line","environments":["dev","prod"]}`)
	wantError(t, recorder, http.StatusConflict, "PROMOTION_ROUTE_CONFLICT")

	// A second route to check ordering.
	registerRouteOK(t, router, "alpha-line", []string{"dev"})

	// List is sorted by name lexicographically.
	recorder = doRequest(t, router, http.MethodGet, "/api/v1/promotion-routes", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("list status = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	routes := decodeBody(t, recorder)["promotion_routes"].([]any)
	if len(routes) != 2 || routes[0].(map[string]any)["name"] != "alpha-line" ||
		routes[1].(map[string]any)["name"] != "prod-line" {
		t.Fatalf("routes = %v", routes)
	}

	// Single get by exact name; case is not folded.
	recorder = doRequest(t, router, http.MethodGet, "/api/v1/promotion-routes/prod-line", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("get status = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	if decodeBody(t, recorder)["promotion_route"].(map[string]any)["name"] != "prod-line" {
		t.Fatalf("get body = %s", recorder.Body.String())
	}
	recorder = doRequest(t, router, http.MethodGet, "/api/v1/promotion-routes/Prod-Line", "")
	wantError(t, recorder, http.StatusNotFound, "PROMOTION_ROUTE_NOT_FOUND")
	recorder = doRequest(t, router, http.MethodGet, "/api/v1/promotion-routes/missing", "")
	wantError(t, recorder, http.StatusNotFound, "PROMOTION_ROUTE_NOT_FOUND")
}

func TestPromotionRouteValidationFailures(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "dev")
	registerEnvironmentOK(t, router, "test")

	cases := []struct {
		name string
		body string
	}{
		{"missing name", `{"environments":["dev"]}`},
		{"blank name", `{"name":"   ","environments":["dev"]}`},
		{"name too long", `{"name":"` + longName(129) + `","environments":["dev"]}`},
		{"missing environments", `{"name":"r1"}`},
		{"empty environments", `{"name":"r1","environments":[]}`},
		{"blank environment", `{"name":"r1","environments":["dev",""]}`},
		{"repeated environment", `{"name":"r1","environments":["dev","dev"]}`},
		{"wrong type", `{"name":123,"environments":["dev"]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := doRequest(t, router, http.MethodPost, "/api/v1/promotion-routes", tc.body)
			wantError(t, recorder, http.StatusUnprocessableEntity, "PROMOTION_ROUTE_VALIDATION_FAILED")
		})
	}

	// Unknown environment is 404, checked after shape validation.
	recorder := doRequest(t, router, http.MethodPost, "/api/v1/promotion-routes",
		`{"name":"r1","environments":["dev","ghost"]}`)
	wantError(t, recorder, http.StatusNotFound, "ENVIRONMENT_NOT_FOUND")

	// Invalid JSON is 400 invalid_request.
	recorder = doRequest(t, router, http.MethodPost, "/api/v1/promotion-routes", `{not json`)
	wantError(t, recorder, http.StatusBadRequest, "invalid_request")
}

func longName(n int) string {
	buf := make([]byte, n)
	for i := range buf {
		buf[i] = 'x'
	}
	return string(buf)
}

func TestPromotionRouteNameUniquenessAfterTrim(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "dev")
	registerRouteOK(t, router, "line", []string{"dev"})
	// Surrounding whitespace folds onto the same stored key; identical
	// sequence is idempotent.
	recorder := doRequest(t, router, http.MethodPost, "/api/v1/promotion-routes",
		`{"name":"  line  ","environments":["dev"]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("trimmed same-name status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
}

func TestBindBatchToPromotionRoute(t *testing.T) {
	router := newTestRouter(t)
	for _, env := range []string{"dev", "prod"} {
		registerEnvironmentOK(t, router, env)
	}
	registerRouteOK(t, router, "prod-line", []string{"dev", "prod"})
	registerRouteOK(t, router, "other-line", []string{"dev"})
	batch := "batch-bind"
	createRecordOK(t, router, batchRecordBody(batch, "dev", "1.0.0", "allowed", "rb:0.9.0", "c"))

	bind := func(body string) *httptest.ResponseRecorder {
		return doRequest(t, router, http.MethodPut,
			"/api/v1/release-batches/"+batch+"/promotion-route", body)
	}

	// First binding 201.
	recorder := bind(`{"route":"prod-line"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("first bind status = %d, want 201 (body %s)", recorder.Code, recorder.Body.String())
	}
	binding := decodeBody(t, recorder)
	if binding["batch_id"] != batch || binding["route"] != "prod-line" || len(binding) != 2 {
		t.Fatalf("binding = %v", binding)
	}

	// Same route 200.
	recorder = bind(`{"route":"prod-line"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("same route status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}

	// Different route 409.
	recorder = bind(`{"route":"other-line"}`)
	wantError(t, recorder, http.StatusConflict, "PROMOTION_ROUTE_ALREADY_BOUND")

	// Unknown route 404, even though the batch exists.
	recorder = bind(`{"route":"ghost-line"}`)
	wantError(t, recorder, http.StatusNotFound, "PROMOTION_ROUTE_NOT_FOUND")

	// Unknown batch 404 (route exists).
	recorder = doRequest(t, router, http.MethodPut,
		"/api/v1/release-batches/ghost-batch/promotion-route", `{"route":"prod-line"}`)
	wantError(t, recorder, http.StatusNotFound, "RELEASE_BATCH_NOT_FOUND")

	// Field errors 422.
	for _, body := range []string{`{}`, `{"route":""}`, `{"route":"   "}`, `{"route":123}`} {
		recorder = bind(body)
		wantError(t, recorder, http.StatusUnprocessableEntity, "PROMOTION_ROUTE_BINDING_VALIDATION_FAILED")
	}
	// Unknown route still beats validation on non-blank values: covered above.
	recorder = bind(`{broken`)
	wantError(t, recorder, http.StatusBadRequest, "invalid_request")

	// Unknown route with unknown batch: route read happens before batch check.
	recorder = doRequest(t, router, http.MethodPut,
		"/api/v1/release-batches/ghost-batch/promotion-route", `{"route":"ghost-line"}`)
	wantError(t, recorder, http.StatusNotFound, "PROMOTION_ROUTE_NOT_FOUND")
}
