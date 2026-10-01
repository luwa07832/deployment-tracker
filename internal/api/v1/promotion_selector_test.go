package v1

import (
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
)

func setupRoutedBatch(t *testing.T, router *gin.Engine, batch string) {
	t.Helper()
	for _, env := range []string{"dev", "test", "prod"} {
		registerEnvironmentOK(t, router, env)
	}
	registerRouteOK(t, router, "prod-line", []string{"dev", "test", "prod"})
	// dev and test carry the change consistently; prod is missing it.
	createRecordOK(t, router, batchRecordBody(batch, "dev", "1.0.0", "allowed", "rb:0.9.0", "alpha"))
	createRecordOK(t, router, batchRecordBody(batch, "test", "1.0.0", "allowed", "rb:0.9.0", "alpha"))
	createRecordOK(t, router, batchRecordBody(batch, "prod", "1.0.1", "blocked", "rb:1.0.0", "other"))
}

func TestPromotionChainAcceptsRouteSelector(t *testing.T) {
	router := newTestRouter(t)
	batch := "batch-route"
	setupRoutedBatch(t, router, batch)

	recorder := doRequest(t, router, http.MethodGet,
		"/api/v1/release-batches/"+batch+"/promotion-chain?route=prod-line", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("route chain status = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	body := decodeBody(t, recorder)
	envs := body["environments"].([]any)
	if len(envs) != 3 || envs[0] != "dev" || envs[2] != "prod" {
		t.Fatalf("environments = %v", envs)
	}
	if body["consistent"] != false {
		t.Fatalf("consistent = %v, want false", body["consistent"])
	}

	// Bound route is used when no selector is present.
	recorder = doRequest(t, router, http.MethodPut,
		"/api/v1/release-batches/"+batch+"/promotion-route", `{"route":"prod-line"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("bind status = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	recorder = doRequest(t, router, http.MethodGet,
		"/api/v1/release-batches/"+batch+"/promotion-chain", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("bound chain status = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	body = decodeBody(t, recorder)
	if len(body["environments"].([]any)) != 3 || body["consistent"] != false {
		t.Fatalf("bound chain body = %v", body)
	}

	// Explicit environments still override and are not merged with the route.
	recorder = doRequest(t, router, http.MethodGet,
		"/api/v1/release-batches/"+batch+"/promotion-chain?environments=dev,test", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("explicit env chain status = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	if len(decodeBody(t, recorder)["environments"].([]any)) != 2 {
		t.Fatalf("explicit env body = %s", recorder.Body.String())
	}
}

func TestPromotionSelectorConflictsAndFallbacks(t *testing.T) {
	router := newTestRouter(t)
	batch := "batch-sel"
	setupRoutedBatch(t, router, batch)

	chain := "/api/v1/release-batches/" + batch + "/promotion-chain"
	trace := "/api/v1/release-batches/" + batch + "/changes/alpha/trace"

	// Both selectors present -> 400 PROMOTION_SELECTOR_CONFLICT on both reads.
	for _, target := range []string{
		chain + "?environments=dev&route=prod-line",
		trace + "?environments=dev&route=prod-line",
	} {
		recorder := doRequest(t, router, http.MethodGet, target, "")
		wantError(t, recorder, http.StatusBadRequest, "PROMOTION_SELECTOR_CONFLICT")
	}

	// Unknown route -> 404, before the batch check.
	recorder := doRequest(t, router, http.MethodGet, chain+"?route=ghost-line", "")
	wantError(t, recorder, http.StatusNotFound, "PROMOTION_ROUTE_NOT_FOUND")

	// Unbound batch with no selector -> 400 INVALID_PROMOTION_SEQUENCE.
	recorder = doRequest(t, router, http.MethodGet, chain, "")
	wantError(t, recorder, http.StatusBadRequest, "INVALID_PROMOTION_SEQUENCE")
	recorder = doRequest(t, router, http.MethodGet, trace, "")
	wantError(t, recorder, http.StatusBadRequest, "INVALID_PROMOTION_SEQUENCE")

	// Selector / binding resolution precedes the batch existence check.
	ghostChain := "/api/v1/release-batches/ghost/promotion-chain"
	recorder = doRequest(t, router, http.MethodGet, ghostChain+"?environments=dev,dev", "")
	wantError(t, recorder, http.StatusBadRequest, "INVALID_PROMOTION_SEQUENCE")
	recorder = doRequest(t, router, http.MethodGet, ghostChain+"?environments=dev,ghost", "")
	wantError(t, recorder, http.StatusBadRequest, "INVALID_PROMOTION_SEQUENCE")
	recorder = doRequest(t, router, http.MethodGet, ghostChain+"?route=ghost-line", "")
	wantError(t, recorder, http.StatusNotFound, "PROMOTION_ROUTE_NOT_FOUND")
	recorder = doRequest(t, router, http.MethodGet, ghostChain, "")
	wantError(t, recorder, http.StatusBadRequest, "INVALID_PROMOTION_SEQUENCE")
	recorder = doRequest(t, router, http.MethodGet, ghostChain+"?route=prod-line", "")
	wantError(t, recorder, http.StatusNotFound, "RELEASE_BATCH_NOT_FOUND")
}

func TestTraceUsesRouteAndBinding(t *testing.T) {
	router := newTestRouter(t)
	batch := "batch-trace"
	setupRoutedBatch(t, router, batch)

	trace := "/api/v1/release-batches/" + batch + "/changes/alpha/trace"
	recorder := doRequest(t, router, http.MethodGet, trace+"?route=prod-line", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("route trace status = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	body := decodeBody(t, recorder)
	if body["first_environment"] != "dev" {
		t.Fatalf("first_environment = %v", body["first_environment"])
	}
	passed := body["passed_environments"].([]any)
	if len(passed) != 2 || passed[0] != "dev" || passed[1] != "test" {
		t.Fatalf("passed_environments = %v", passed)
	}
	if body["first_missing_environment"] != "prod" {
		t.Fatalf("first_missing_environment = %v", body["first_missing_environment"])
	}
	envs := body["environments"].([]any)
	if len(envs) != 3 || envs[2] != "prod" {
		t.Fatalf("environments = %v", envs)
	}

	// Bind, then the trace works without any selector.
	recorder = doRequest(t, router, http.MethodPut,
		"/api/v1/release-batches/"+batch+"/promotion-route", `{"route":"prod-line"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("bind status = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	recorder = doRequest(t, router, http.MethodGet, trace, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("bound trace status = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	if decodeBody(t, recorder)["first_missing_environment"] != "prod" {
		t.Fatalf("bound trace body = %s", recorder.Body.String())
	}
}

func TestPromotionDiffIgnoresRouteSelector(t *testing.T) {
	router := newTestRouter(t)
	batch := "batch-diff-route"
	setupRoutedBatch(t, router, batch)
	// promotion-diff keeps its explicit from/to contract and does not take a
	// route; a bound route does not change its behavior either.
	recorder := doRequest(t, router, http.MethodPut,
		"/api/v1/release-batches/"+batch+"/promotion-route", `{"route":"prod-line"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("bind status = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	recorder = doRequest(t, router, http.MethodGet,
		"/api/v1/release-batches/"+batch+"/promotion-diff?from=dev&to=prod", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("diff status = %d (body %s)", recorder.Code, recorder.Body.String())
	}
}
