package v1

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/luwa07832/deployment-tracker/internal/store"
)

func newReleaseBatchesTestRouter(t *testing.T) (*gin.Engine, *store.Store) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := store.Open(filepath.Join(t.TempDir(), "release-batches.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	engine := gin.New()
	Register(engine, Dependencies{Store: db})
	return engine, db
}

// batchRecordBody builds a release-records body carrying a batch id.
func discoveryRecordBody(environment, version, batch, gate string, changes ...map[string]any) string {
	record := map[string]any{
		"environment":    environment,
		"version":        version,
		"batch_id":       batch,
		"gate_status":    gate,
		"rollback_point": "rb:" + version,
		"changes":        changes,
	}
	raw, _ := json.Marshal(record)
	return string(raw)
}

func pinBatchRecordedAt(t *testing.T, db *store.Store, version, recordedAt string) {
	t.Helper()
	records, err := db.ListReleaseRecords(store.ReleaseRecordFilter{Version: version})
	if err != nil || len(records) == 0 {
		t.Fatalf("locate %s: %+v %v", version, records, err)
	}
	for i := range records {
		if err := db.SetRecordedAtForTest(records[i].ID, recordedAt); err != nil {
			t.Fatalf("pin recorded_at: %v", err)
		}
	}
}

// seedBatchDiscovery writes two batches with deterministic timestamps:
// b-zeta spans dev/prod (3 releases, 4 changes, one of each gate status);
// b-alpha has a single dev record. One unbatched prod record exists too.
func seedBatchDiscovery(t *testing.T, router *gin.Engine, db *store.Store) {
	t.Helper()
	registerEnvironmentOK(t, router, "dev")
	registerEnvironmentOK(t, router, "prod")
	createRecordOK(t, router, discoveryRecordBody("dev", "1.0.0", "b-zeta", "allowed",
		entryMap(1, "feature", "alpha", "alpha desc"),
		entryMap(2, "fix", "beta", "beta desc")))
	pinBatchRecordedAt(t, db, "1.0.0", "2026-10-01T08:00:00Z")
	createRecordOK(t, router, discoveryRecordBody("dev", "1.1.0", "b-zeta", "blocked",
		entryMap(1, "feature", "gamma", "gamma desc")))
	pinBatchRecordedAt(t, db, "1.1.0", "2026-10-01T09:00:00Z")
	createRecordOK(t, router, discoveryRecordBody("prod", "1.1.0", "b-zeta", "pending",
		entryMap(1, "ops", "delta", "delta desc")))
	pinBatchRecordedAt(t, db, "1.1.0", "2026-10-01T09:00:00Z")
	createRecordOK(t, router, discoveryRecordBody("dev", "2.0.0", "b-alpha", "allowed",
		entryMap(1, "feature", "epsilon", "epsilon desc")))
	pinBatchRecordedAt(t, db, "2.0.0", "2026-10-03T08:00:00Z")
	createRecordOK(t, router, changeRecordBody("prod", "9.0.0", "allowed", "rb:9", "",
		entryMap(1, "ops", "unbatched", "never in a batch")))
	pinBatchRecordedAt(t, db, "9.0.0", "2026-10-04T08:00:00Z")
}

func decodeBatchesResponse(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	body := decodeBody(t, recorder)
	for _, key := range []string{"batches", "next_cursor"} {
		if _, ok := body[key]; !ok {
			t.Fatalf("response missing %s: %s", key, recorder.Body.String())
		}
	}
	if len(body) != 2 {
		t.Fatalf("response must contain only batches and next_cursor: %s", recorder.Body.String())
	}
	return body
}

func batchViews(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, ok := body["batches"].([]any)
	if !ok {
		t.Fatalf("batches is not an array: %v", body["batches"])
	}
	batches := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		batch, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("batch is not an object: %v", item)
		}
		batches = append(batches, batch)
	}
	return batches
}

func TestListReleaseBatchesDiscoverySummary(t *testing.T) {
	router, db := newReleaseBatchesTestRouter(t)

	seedBatchDiscovery(t, router, db)

	recorder := doRequest(t, router, http.MethodGet, "/api/v1/release-batches", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, %s", recorder.Code, recorder.Body.String())
	}
	body := decodeBatchesResponse(t, recorder)
	if body["next_cursor"] != nil {
		t.Fatalf("final page next_cursor = %v, want null", body["next_cursor"])
	}
	batches := batchViews(t, body)
	if len(batches) != 2 {
		t.Fatalf("two batches (unbatched excluded), got %d: %s", len(batches), recorder.Body.String())
	}
	if batches[0]["batch_id"] != "b-alpha" || batches[1]["batch_id"] != "b-zeta" {
		t.Fatalf("order = %v, %v; want newest last_recorded_at first", batches[0]["batch_id"], batches[1]["batch_id"])
	}
	if batches[0]["last_recorded_at"] != "2026-10-03T08:00:00Z" {
		t.Fatalf("last_recorded_at = %v", batches[0]["last_recorded_at"])
	}

	zeta := batches[1]
	if int(zeta["release_count"].(float64)) != 3 {
		t.Fatalf("release_count = %v, want 3", zeta["release_count"])
	}
	if int(zeta["change_count"].(float64)) != 4 {
		t.Fatalf("change_count = %v, want 4 across every batch record", zeta["change_count"])
	}
	environments, _ := zeta["environments"].([]any)
	if len(environments) != 2 || environments[0] != "dev" || environments[1] != "prod" {
		t.Fatalf("environments = %v, want sorted unique [dev prod]", environments)
	}
	gateCounts, _ := zeta["gate_counts"].(map[string]any)
	if gateCounts["allowed"] != float64(1) || gateCounts["blocked"] != float64(1) || gateCounts["pending"] != float64(1) {
		t.Fatalf("gate_counts = %v, want 1/1/1", gateCounts)
	}
}

func TestListReleaseBatchesEmptyResult(t *testing.T) {
	router, _ := newReleaseBatchesTestRouter(t)
	registerEnvironmentOK(t, router, "prod")

	recorder := doRequest(t, router, http.MethodGet, "/api/v1/release-batches?environment=prod", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, %s", recorder.Code, recorder.Body.String())
	}
	body := decodeBatchesResponse(t, recorder)
	if body["next_cursor"] != nil {
		t.Fatalf("empty result next_cursor = %v, want null", body["next_cursor"])
	}
	if !strings.Contains(recorder.Body.String(), `"batches":[]`) {
		t.Fatalf("empty result must serialize as []: %s", recorder.Body.String())
	}
}

func TestListReleaseBatchesFiltersAreAND(t *testing.T) {
	router, db := newReleaseBatchesTestRouter(t)
	seedBatchDiscovery(t, router, db)

	// gate_status blocked only exists on dev: dev+blocked keeps b-zeta,
	// prod+blocked excludes it, and version 1.0.0 + blocked excludes it too.
	cases := []struct {
		query string
		want  []string
	}{
		{"gate_status=blocked", []string{"b-zeta"}},
		{"gate_status=allowed", []string{"b-alpha", "b-zeta"}},
		{"environment=prod&gate_status=blocked", []string{}},
		{"version=1.1.0&gate_status=allowed", []string{}},
		{"version=1.1.0", []string{"b-zeta"}},
		{"from=2026-10-03&to=2026-10-03T23:59:59Z", []string{"b-alpha"}},
		{"from=2026-10-01T09:00:00Z&to=2026-10-01T09:00:00Z", []string{"b-zeta"}},
		{"environment=prod", []string{"b-zeta"}},
	}
	for _, tc := range cases {
		recorder := doRequest(t, router, http.MethodGet, "/api/v1/release-batches?"+tc.query, "")
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s status = %d, %s", tc.query, recorder.Code, recorder.Body.String())
		}
		got := []string{}
		for _, batch := range batchViews(t, decodeBatchesResponse(t, recorder)) {
			got = append(got, batch["batch_id"].(string))
		}
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Fatalf("%s = %v, want %v", tc.query, got, tc.want)
		}
	}

	// A filter match only qualifies the batch; its summary covers all records.
	recorder := doRequest(t, router, http.MethodGet,
		"/api/v1/release-batches?gate_status=blocked", "")
	batches := batchViews(t, decodeBatchesResponse(t, recorder))
	if len(batches) != 1 || int(batches[0]["release_count"].(float64)) != 3 {
		t.Fatalf("filtered summary must cover every batch record: %s", recorder.Body.String())
	}
}

func TestListReleaseBatchesPaginationWalk(t *testing.T) {
	router, db := newReleaseBatchesTestRouter(t)
	seedBatchDiscovery(t, router, db)

	path := "/api/v1/release-batches?limit=1"
	seen := []string{}
	for page := 0; page < 5; page++ {
		recorder := doRequest(t, router, http.MethodGet, path, "")
		if recorder.Code != http.StatusOK {
			t.Fatalf("page %d status = %d, %s", page, recorder.Code, recorder.Body.String())
		}
		body := decodeBatchesResponse(t, recorder)
		batches := batchViews(t, body)
		if len(batches) > 1 {
			t.Fatalf("page %d returned %d batches", page, len(batches))
		}
		seen = append(seen, batches[0]["batch_id"].(string))
		next, _ := body["next_cursor"].(string)
		if next == "" {
			if body["next_cursor"] != nil {
				t.Fatalf("non-final cursor was not a string: %v", body["next_cursor"])
			}
			break
		}
		path = "/api/v1/release-batches?limit=1&cursor=" + next
	}
	if strings.Join(seen, ",") != "b-alpha,b-zeta" {
		t.Fatalf("walk = %v, want b-alpha then b-zeta", seen)
	}
}

func TestListReleaseBatchesQueryValidation(t *testing.T) {
	router, _ := newReleaseBatchesTestRouter(t)
	registerEnvironmentOK(t, router, "prod")
	invalid := []string{
		"/api/v1/release-batches?limit=",
		"/api/v1/release-batches?limit=abc",
		"/api/v1/release-batches?limit=0",
		"/api/v1/release-batches?limit=101",
		"/api/v1/release-batches?limit=-1",
		"/api/v1/release-batches?limit=1.5",
		"/api/v1/release-batches?cursor=",
		"/api/v1/release-batches?gate_status=unknown",
		"/api/v1/release-batches?from=not-a-time",
		"/api/v1/release-batches?to=2026-13-40",
		"/api/v1/release-batches?cursor=bogus",
		"/api/v1/release-batches?cursor=YWJj.AAAA",
	}
	for _, path := range invalid {
		recorder := doRequest(t, router, http.MethodGet, path, "")
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d, want 400 (%s)", path, recorder.Code, recorder.Body.String())
		}
		wantError(t, recorder, http.StatusBadRequest, store.CodeBatchQueryInvalidV1)
	}
}

func TestListReleaseBatchesCursorBinding(t *testing.T) {
	router, db := newReleaseBatchesTestRouter(t)
	seedBatchDiscovery(t, router, db)

	first := doRequest(t, router, http.MethodGet,
		"/api/v1/release-batches?gate_status=allowed&limit=1", "")
	body := decodeBatchesResponse(t, first)
	cursor, _ := body["next_cursor"].(string)
	if cursor == "" {
		t.Fatal("expected a next_cursor")
	}

	// Same filters: the cursor resolves to the second batch.
	match := doRequest(t, router, http.MethodGet,
		"/api/v1/release-batches?gate_status=allowed&limit=1&cursor="+cursor, "")
	if match.Code != http.StatusOK {
		t.Fatalf("bound cursor replay = %d, %s", match.Code, match.Body.String())
	}
	batches := batchViews(t, decodeBatchesResponse(t, match))
	if len(batches) != 1 || batches[0]["batch_id"] != "b-zeta" {
		t.Fatalf("bound cursor page = %s", match.Body.String())
	}

	// Different filters: the cursor binding no longer matches.
	for _, query := range []string{
		"/api/v1/release-batches?limit=1&cursor=" + cursor,
		"/api/v1/release-batches?gate_status=blocked&limit=1&cursor=" + cursor,
		"/api/v1/release-batches?gate_status=allowed&environment=dev&limit=1&cursor=" + cursor,
		"/api/v1/release-batches?gate_status=allowed&from=2026-10-01&limit=1&cursor=" + cursor,
	} {
		recorder := doRequest(t, router, http.MethodGet, query, "")
		wantError(t, recorder, http.StatusBadRequest, store.CodeBatchQueryInvalidV1)
	}

	// Re-signed payload with a mutated filter but valid base64 shape is
	// rejected because the HMAC no longer matches.
	parts := strings.Split(cursor, ".")
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	var marker map[string]any
	if err := json.Unmarshal(payload, &marker); err != nil {
		t.Fatal(err)
	}
	marker["batch_id"] = "other"
	mutated, _ := json.Marshal(marker)
	forged := base64.RawURLEncoding.EncodeToString(mutated) + "." + parts[1]
	recorder := doRequest(t, router, http.MethodGet,
		"/api/v1/release-batches?gate_status=allowed&limit=1&cursor="+forged, "")
	wantError(t, recorder, http.StatusBadRequest, store.CodeBatchQueryInvalidV1)
}

func TestListReleaseBatchesUnknownEnvironment(t *testing.T) {
	router, _ := newReleaseBatchesTestRouter(t)
	recorder := doRequest(t, router, http.MethodGet,
		"/api/v1/release-batches?environment=ghost", "")
	wantError(t, recorder, http.StatusNotFound, store.CodeEnvironmentNotFoundV1)
}

func TestGetReleaseBatchDetail(t *testing.T) {
	router, db := newReleaseBatchesTestRouter(t)
	seedBatchDiscovery(t, router, db)

	recorder := doRequest(t, router, http.MethodGet, "/api/v1/release-batches/b-zeta", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, %s", recorder.Code, recorder.Body.String())
	}
	body := decodeBody(t, recorder)
	batch, ok := body["release_batch"].(map[string]any)
	if !ok || len(body) != 1 {
		t.Fatalf("response must wrap one release_batch: %s", recorder.Body.String())
	}
	if batch["batch_id"] != "b-zeta" {
		t.Fatalf("batch_id = %v", batch["batch_id"])
	}
	if int(batch["release_count"].(float64)) != 3 || int(batch["change_count"].(float64)) != 4 {
		t.Fatalf("summary = %v", batch)
	}
	environments, _ := batch["environments"].([]any)
	if environments[0] != "dev" || environments[1] != "prod" {
		t.Fatalf("environments = %v", environments)
	}
	releases, _ := batch["releases"].([]any)
	if len(releases) != 3 {
		t.Fatalf("releases = %d, want all 3 complete records", len(releases))
	}
	// Chronological: 08:00 first; same-instant 09:00 pair ordered by id desc
	// (prod 1.1.0 was inserted after dev 1.1.0).
	first := releases[0].(map[string]any)
	second := releases[1].(map[string]any)
	third := releases[2].(map[string]any)
	if first["recorded_at"] != "2026-10-01T08:00:00Z" || first["version"] != "1.0.0" {
		t.Fatalf("first release = %v", first)
	}
	if second["recorded_at"] != "2026-10-01T09:00:00Z" || second["environment"] != "prod" {
		t.Fatalf("same-instant tie must order id desc: %v", second)
	}
	if third["environment"] != "dev" || third["version"] != "1.1.0" {
		t.Fatalf("third release = %v", third)
	}
	changes, _ := first["changes"].([]any)
	if len(changes) != 2 || changes[0].(map[string]any)["title"] != "alpha" {
		t.Fatalf("complete records must keep changes in stored order: %v", changes)
	}
}

func TestGetReleaseBatchNotFound(t *testing.T) {
	router, _ := newReleaseBatchesTestRouter(t)
	recorder := doRequest(t, router, http.MethodGet, "/api/v1/release-batches/missing", "")
	wantError(t, recorder, http.StatusNotFound, store.CodeReleaseBatchNotFoundV1)
}
