package v1

import (
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/luwa07832/deployment-tracker/internal/store"
)

func createRouteOK(t *testing.T, router *gin.Engine, name string, environments ...string) {
	t.Helper()
	quoted := make([]string, 0, len(environments))
	for _, env := range environments {
		quoted = append(quoted, `"`+env+`"`)
	}
	body := `{"name":"` + name + `","environments":[` + strings.Join(quoted, ",") + `]}`
	recorder := doRequest(t, router, http.MethodPost, "/api/v1/promotion-routes", body)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create route %s status = %d, want 201 (body %s)", name, recorder.Code, recorder.Body.String())
	}
}

func TestPromotionRouteCreateListAndGet(t *testing.T) {
	router := newTestRouter(t)
	for _, env := range []string{"dev", "test", "staging", "prod"} {
		registerEnvironmentOK(t, router, env)
	}
	recorder := doRequest(t, router, http.MethodPost, "/api/v1/promotion-routes",
		`{"name":" prod-line ","environments":["dev","test","staging","prod"]}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201 (body %s)", recorder.Code, recorder.Body.String())
	}
	body := decodeBody(t, recorder)
	route := body["promotion_route"].(map[string]any)
	if route["name"] != "prod-line" {
		t.Fatalf("name = %v, want trimmed prod-line", route["name"])
	}
	envs := route["environments"].([]any)
	if len(envs) != 4 || envs[0] != "dev" || envs[3] != "prod" {
		t.Fatalf("environments = %v", envs)
	}

	// Identical sequence is idempotent: 200 with the same route.
	recorder = doRequest(t, router, http.MethodPost, "/api/v1/promotion-routes",
		`{"name":"prod-line","environments":["dev","test","staging","prod"]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("re-create status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	again := decodeBody(t, recorder)["promotion_route"].(map[string]any)
	if again["name"] != "prod-line" {
		t.Fatalf("idempotent re-create changed the route: %v", again)
	}

	// A different sequence under the same name conflicts.
	wantError(t, doRequest(t, router, http.MethodPost, "/api/v1/promotion-routes",
		`{"name":"prod-line","environments":["dev","prod"]}`),
		http.StatusConflict, store.CodePromotionRouteConflictV1)

	// A second route sorts before the first by name.
	createRouteOK(t, router, "alpha-line", "dev", "prod")
	recorder = doRequest(t, router, http.MethodGet, "/api/v1/promotion-routes", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("list status = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	routes := decodeBody(t, recorder)["promotion_routes"].([]any)
	if len(routes) != 2 {
		t.Fatalf("promotion_routes = %v", routes)
	}
	first := routes[0].(map[string]any)
	second := routes[1].(map[string]any)
	if first["name"] != "alpha-line" || second["name"] != "prod-line" {
		t.Fatalf("list order = %v, %v; want name ascending", first["name"], second["name"])
	}
	if envs := second["environments"].([]any); len(envs) != 4 {
		t.Fatalf("listed environments = %v", envs)
	}

	// Single lookup by exact name.
	recorder = doRequest(t, router, http.MethodGet, "/api/v1/promotion-routes/prod-line", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("get status = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	got := decodeBody(t, recorder)["promotion_route"].(map[string]any)
	if got["name"] != "prod-line" {
		t.Fatalf("got %v", got)
	}
}

func TestPromotionRouteValidationAndNotFound(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "dev")
	registerEnvironmentOK(t, router, "prod")

	cases := []struct {
		name string
		body string
	}{
		{"missing name", `{"environments":["dev"]}`},
		{"blank name", `{"name":"  ","environments":["dev"]}`},
		{"too long name", `{"name":"` + strings.Repeat("x", 129) + `","environments":["dev"]}`},
		{"missing environments", `{"name":"r1"}`},
		{"empty environments", `{"name":"r1","environments":[]}`},
		{"empty entry", `{"name":"r1","environments":["dev","  "]}`},
		{"duplicate environments", `{"name":"r1","environments":["dev","prod","dev"]}`},
		{"name wrong type", `{"name":7,"environments":["dev"]}`},
		{"environments wrong type", `{"name":"r1","environments":"dev"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantError(t, doRequest(t, router, http.MethodPost, "/api/v1/promotion-routes", tc.body),
				http.StatusUnprocessableEntity, store.CodePromotionRouteValidationV1)
		})
	}

	// Unknown environment keys are 404, not 422.
	wantError(t, doRequest(t, router, http.MethodPost, "/api/v1/promotion-routes",
		`{"name":"r1","environments":["dev","ghost"]}`),
		http.StatusNotFound, store.CodeEnvironmentNotFoundV1)

	// Malformed JSON is the shared 400.
	wantError(t, doRequest(t, router, http.MethodPost, "/api/v1/promotion-routes", `{not json`),
		http.StatusBadRequest, store.CodeInvalidRequest)

	// Unknown, blank and case-mismatched names are all plain not-found.
	wantError(t, doRequest(t, router, http.MethodGet, "/api/v1/promotion-routes/nope", ""),
		http.StatusNotFound, store.CodePromotionRouteNotFoundV1)
	wantError(t, doRequest(t, router, http.MethodGet, "/api/v1/promotion-routes/%20", ""),
		http.StatusNotFound, store.CodePromotionRouteNotFoundV1)
	createRouteOK(t, router, "Prod-Line", "dev")
	wantError(t, doRequest(t, router, http.MethodGet, "/api/v1/promotion-routes/prod-line", ""),
		http.StatusNotFound, store.CodePromotionRouteNotFoundV1)
}

func setupBoundBatch(t *testing.T, router *gin.Engine) string {
	t.Helper()
	for _, env := range []string{"dev", "test", "staging", "prod"} {
		registerEnvironmentOK(t, router, env)
	}
	createRouteOK(t, router, "prod-line", "dev", "test", "staging", "prod")
	batch := "batch-bound"
	for _, env := range []string{"dev", "test", "staging", "prod"} {
		createRecordOK(t, router, batchRecordBody(batch, env, "1.0.0", "allowed", "rb:0.9.0", "add login"))
	}
	return batch
}

func TestBindPromotionRouteLifecycle(t *testing.T) {
	router := newTestRouter(t)
	batch := setupBoundBatch(t, router)

	// First bind: 201 with batch_id and route.
	recorder := doRequest(t, router, http.MethodPut,
		"/api/v1/release-batches/"+batch+"/promotion-route", `{"route":"prod-line"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("bind status = %d, want 201 (body %s)", recorder.Code, recorder.Body.String())
	}
	body := decodeBody(t, recorder)
	if body["batch_id"] != batch || body["route"] != "prod-line" {
		t.Fatalf("bind body = %v", body)
	}

	// Repeating the same route is idempotent.
	recorder = doRequest(t, router, http.MethodPut,
		"/api/v1/release-batches/"+batch+"/promotion-route", `{"route":"prod-line"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("re-bind status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}

	// Binding a different route conflicts.
	createRouteOK(t, router, "hotfix-line", "dev", "prod")
	wantError(t, doRequest(t, router, http.MethodPut,
		"/api/v1/release-batches/"+batch+"/promotion-route", `{"route":"hotfix-line"}`),
		http.StatusConflict, store.CodePromotionRouteAlreadyBoundV1)

	// Unknown route, unknown batch, field errors, malformed JSON.
	wantError(t, doRequest(t, router, http.MethodPut,
		"/api/v1/release-batches/"+batch+"/promotion-route", `{"route":"ghost"}`),
		http.StatusNotFound, store.CodePromotionRouteNotFoundV1)
	wantError(t, doRequest(t, router, http.MethodPut,
		"/api/v1/release-batches/batch-nope/promotion-route", `{"route":"prod-line"}`),
		http.StatusNotFound, store.CodeReleaseBatchNotFoundV1)
	wantError(t, doRequest(t, router, http.MethodPut,
		"/api/v1/release-batches/"+batch+"/promotion-route", `{"route":"  "}`),
		http.StatusUnprocessableEntity, store.CodePromotionRouteBindingValidationV1)
	wantError(t, doRequest(t, router, http.MethodPut,
		"/api/v1/release-batches/"+batch+"/promotion-route", `{}`),
		http.StatusUnprocessableEntity, store.CodePromotionRouteBindingValidationV1)
	wantError(t, doRequest(t, router, http.MethodPut,
		"/api/v1/release-batches/"+batch+"/promotion-route", `{"route":5}`),
		http.StatusUnprocessableEntity, store.CodePromotionRouteBindingValidationV1)
	wantError(t, doRequest(t, router, http.MethodPut,
		"/api/v1/release-batches/"+batch+"/promotion-route", `{nope`),
		http.StatusBadRequest, store.CodeInvalidRequest)
}

func TestPromotionChainUsesRouteSelectorAndBinding(t *testing.T) {
	router := newTestRouter(t)
	batch := setupBoundBatch(t, router)

	// Explicit route selector computes the chain along the route sequence.
	recorder := doRequest(t, router, http.MethodGet,
		"/api/v1/release-batches/"+batch+"/promotion-chain?route=prod-line", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("chain by route status = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	body := decodeBody(t, recorder)
	envs := body["environments"].([]any)
	if len(envs) != 4 || envs[0] != "dev" || envs[3] != "prod" {
		t.Fatalf("environments = %v", envs)
	}
	if body["consistent"] != true {
		t.Fatalf("consistent = %v, want true", body["consistent"])
	}

	// Without any selector the bound route supplies the sequence.
	doRequest(t, router, http.MethodPut,
		"/api/v1/release-batches/"+batch+"/promotion-route", `{"route":"prod-line"}`)
	recorder = doRequest(t, router, http.MethodGet,
		"/api/v1/release-batches/"+batch+"/promotion-chain", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("chain by binding status = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	envs = decodeBody(t, recorder)["environments"].([]any)
	if len(envs) != 4 || envs[1] != "test" {
		t.Fatalf("bound environments = %v", envs)
	}

	// route and environments together conflict, even for an unknown batch.
	wantError(t, doRequest(t, router, http.MethodGet,
		"/api/v1/release-batches/"+batch+"/promotion-chain?route=prod-line&environments=dev,prod", ""),
		http.StatusBadRequest, store.CodePromotionSelectorConflictV1)
	wantError(t, doRequest(t, router, http.MethodGet,
		"/api/v1/release-batches/batch-nope/promotion-chain?route=prod-line&environments=dev,prod", ""),
		http.StatusBadRequest, store.CodePromotionSelectorConflictV1)

	// An unknown route is read before the batch existence check.
	wantError(t, doRequest(t, router, http.MethodGet,
		"/api/v1/release-batches/batch-nope/promotion-chain?route=ghost", ""),
		http.StatusNotFound, store.CodePromotionRouteNotFoundV1)

	// No selector and no binding is still the classic 400.
	wantError(t, doRequest(t, router, http.MethodGet,
		"/api/v1/release-batches/batch-nope/promotion-chain", ""),
		http.StatusBadRequest, store.CodePromotionSequenceInvalidV1)
}

func TestPromotionTraceUsesRouteSelectorAndBinding(t *testing.T) {
	router := newTestRouter(t)
	for _, env := range []string{"dev", "test", "staging", "prod"} {
		registerEnvironmentOK(t, router, env)
	}
	createRouteOK(t, router, "prod-line", "dev", "test", "staging", "prod")
	batch := "batch-trace"
	createRecordOK(t, router, batchRecordBody(batch, "dev", "1.0.0", "allowed", "rb:0.9.0", "beta"))
	createRecordOK(t, router, batchRecordBody(batch, "test", "1.0.0", "allowed", "rb:0.9.0", "beta"))
	createRecordOK(t, router, batchRecordBody(batch, "staging", "1.0.0", "allowed", "rb:0.9.0"))
	createRecordOK(t, router, batchRecordBody(batch, "prod", "1.0.0", "allowed", "rb:0.9.0", "beta"))

	recorder := doRequest(t, router, http.MethodGet,
		"/api/v1/release-batches/"+batch+"/changes/beta/trace?route=prod-line", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("trace by route status = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	body := decodeBody(t, recorder)
	if body["first_environment"] != "dev" || body["first_missing_environment"] != "staging" {
		t.Fatalf("trace = %v", body)
	}
	passed := body["passed_environments"].([]any)
	if len(passed) != 2 || passed[0] != "dev" || passed[1] != "test" {
		t.Fatalf("passed_environments = %v", passed)
	}

	// Bound route drives the trace when no selector is given.
	doRequest(t, router, http.MethodPut,
		"/api/v1/release-batches/"+batch+"/promotion-route", `{"route":"prod-line"}`)
	recorder = doRequest(t, router, http.MethodGet,
		"/api/v1/release-batches/"+batch+"/changes/beta/trace", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("trace by binding status = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	if decodeBody(t, recorder)["first_missing_environment"] != "staging" {
		t.Fatalf("bound trace = %v", decodeBody(t, recorder))
	}

	// Selector conflict and missing selector/binding behave like the chain.
	wantError(t, doRequest(t, router, http.MethodGet,
		"/api/v1/release-batches/"+batch+"/changes/beta/trace?route=prod-line&environments=dev,prod", ""),
		http.StatusBadRequest, store.CodePromotionSelectorConflictV1)
	wantError(t, doRequest(t, router, http.MethodGet,
		"/api/v1/release-batches/batch-nope/changes/beta/trace", ""),
		http.StatusBadRequest, store.CodePromotionSequenceInvalidV1)
}
