package v1

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/luwa07832/deployment-tracker/internal/store"
)

func decodeBatchesBody(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
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

func batchRows(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, ok := body["batches"].([]any)
	if !ok {
		t.Fatalf("batches is not an array: %v", body["batches"])
	}
	rows := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		row, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("batch row is not an object: %v", item)
		}
		rows = append(rows, row)
	}
	return rows
}

func unbatchedRecordBody(environment, version string) string {
	return fmt.Sprintf(
		`{"environment":%q,"version":%q,"changes":[{"sequence":1,"category":"fix","title":"unbatched","description":"x"}],"gate_status":"allowed","rollback_point":"snap:0"}`,
		environment, version)
}

// seedReleaseBatchFixture registers dev/staging/prod and writes:
//
//	batch b-1: prod 1.0.0 allowed (1 change), staging 1.0.0 blocked (1 change)
//	batch b-2: dev 1.0.0 allowed (2 changes), prod 1.0.0 pending (0 changes)
//
// plus one unbatched prod 9.0.0 record that must never appear.
func seedReleaseBatchFixture(t *testing.T, router *gin.Engine) {
	t.Helper()
	for _, env := range []string{"dev", "staging", "prod"} {
		registerEnvironmentOK(t, router, env)
	}
	createRecordOK(t, router, batchRecordBody("b-1", "prod", "1.0.0", "allowed", "snap:0", "alpha"))
	createRecordOK(t, router, batchRecordBody("b-1", "staging", "1.0.0", "blocked", "snap:0", "beta"))
	createRecordOK(t, router, batchRecordBody("b-2", "dev", "1.0.0", "allowed", "snap:0", "alpha", "gamma"))
	createRecordOK(t, router, batchRecordBody("b-2", "prod", "1.0.0", "pending", "snap:0"))
	createRecordOK(t, router, unbatchedRecordBody("prod", "9.0.0"))
}

func TestListReleaseBatchesSummaryAndOrder(t *testing.T) {
	router := newTestRouter(t)
	seedReleaseBatchFixture(t, router)

	recorder := doRequest(t, router, http.MethodGet, "/api/v1/release-batches", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	body := decodeBatchesBody(t, recorder)
	if body["next_cursor"] != nil {
		t.Fatalf("next_cursor must be null on the last page: %v", body["next_cursor"])
	}
	rows := batchRows(t, body)
	if len(rows) != 2 {
		t.Fatalf("batch count = %d, want 2: %v", len(rows), rows)
	}
	if rows[0]["batch_id"] != "b-2" || rows[1]["batch_id"] != "b-1" {
		t.Fatalf("batches must be newest first: %v", rows)
	}
	first := rows[0]
	if first["release_count"].(float64) != 2 {
		t.Fatalf("b-2 release_count = %v", first["release_count"])
	}
	envs, _ := first["environments"].([]any)
	if len(envs) != 2 || envs[0] != "dev" || envs[1] != "prod" {
		t.Fatalf("b-2 environments = %v, want [dev prod]", envs)
	}
	if first["change_count"].(float64) != 2 {
		t.Fatalf("b-2 change_count = %v, want 2", first["change_count"])
	}
	counts, _ := first["gate_counts"].(map[string]any)
	if counts["allowed"].(float64) != 1 || counts["blocked"].(float64) != 0 || counts["pending"].(float64) != 1 {
		t.Fatalf("b-2 gate_counts = %v", counts)
	}
	if _, ok := first["last_recorded_at"].(string); !ok || first["last_recorded_at"] == "" {
		t.Fatalf("b-2 last_recorded_at missing: %v", first)
	}
	if first["last_recorded_at"] != rows[1]["last_recorded_at"] {
		t.Fatalf("fixture batches share a test second; tie order must still be deterministic")
	}
	second := rows[1]
	if second["change_count"].(float64) != 2 {
		t.Fatalf("b-1 change_count = %v, want 2", second["change_count"])
	}
	counts, _ = second["gate_counts"].(map[string]any)
	if counts["allowed"].(float64) != 1 || counts["blocked"].(float64) != 1 || counts["pending"].(float64) != 0 {
		t.Fatalf("b-1 gate_counts = %v", counts)
	}
	envs, _ = second["environments"].([]any)
	if len(envs) != 2 || envs[0] != "prod" || envs[1] != "staging" {
		t.Fatalf("b-1 environments = %v, want [prod staging]", envs)
	}
}

func TestListReleaseBatchesPagination(t *testing.T) {
	router := newTestRouter(t)
	seedReleaseBatchFixture(t, router)

	var seen []string
	path := "/api/v1/release-batches?limit=1"
	for page := 0; page < 5; page++ {
		recorder := doRequest(t, router, http.MethodGet, path, "")
		if recorder.Code != http.StatusOK {
			t.Fatalf("page %d status = %d (body %s)", page, recorder.Code, recorder.Body.String())
		}
		body := decodeBatchesBody(t, recorder)
		rows := batchRows(t, body)
		if len(rows) != 1 {
			t.Fatalf("page %d row count = %d, want 1", page, len(rows))
		}
		seen = append(seen, rows[0]["batch_id"].(string))
		cursor, _ := body["next_cursor"].(string)
		if cursor == "" {
			if body["next_cursor"] != nil {
				t.Fatalf("next_cursor must be nil on the last page: %v", body["next_cursor"])
			}
			break
		}
		path = "/api/v1/release-batches?limit=1&cursor=" + cursor
	}
	if strings.Join(seen, ",") != "b-2,b-1" {
		t.Fatalf("pagination order = %v, want [b-2 b-1]", seen)
	}
}

func TestListReleaseBatchesFilterAndSummaryCoversWholeBatch(t *testing.T) {
	router := newTestRouter(t)
	seedReleaseBatchFixture(t, router)

	// b-1 qualifies through its blocked staging record; the summary must
	// still cover the allowed prod record too.
	recorder := doRequest(t, router, http.MethodGet,
		"/api/v1/release-batches?environment=staging&gate_status=blocked&version=1.0.0", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	rows := batchRows(t, decodeBatchesBody(t, recorder))
	if len(rows) != 1 || rows[0]["batch_id"] != "b-1" {
		t.Fatalf("filter must select only b-1: %v", rows)
	}
	if rows[0]["release_count"].(float64) != 2 {
		t.Fatalf("release_count must cover all b-1 records: %v", rows[0]["release_count"])
	}
	counts, _ := rows[0]["gate_counts"].(map[string]any)
	if counts["allowed"].(float64) != 1 || counts["blocked"].(float64) != 1 {
		t.Fatalf("gate_counts must cover all b-1 records: %v", counts)
	}
	envs, _ := rows[0]["environments"].([]any)
	if len(envs) != 2 {
		t.Fatalf("environments must cover all b-1 records: %v", envs)
	}

	// AND semantics: no batch has a blocked record at prod.
	recorder = doRequest(t, router, http.MethodGet,
		"/api/v1/release-batches?environment=prod&gate_status=blocked", "")
	if rows := batchRows(t, decodeBatchesBody(t, recorder)); len(rows) != 0 {
		t.Fatalf("AND filter must yield no batches: %v", rows)
	}
}

func TestListReleaseBatchesEmptyShape(t *testing.T) {
	router := newTestRouter(t)
	seedReleaseBatchFixture(t, router)

	// Registered environment with no batches is a 200 empty page.
	recorder := doRequest(t, router, http.MethodGet,
		"/api/v1/release-batches?environment=dev&from=3000-01-01T00:00:00Z", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	body := decodeBatchesBody(t, recorder)
	rawBatches, _ := body["batches"].([]any)
	if len(rawBatches) != 0 {
		t.Fatalf("batches must be empty: %v", rawBatches)
	}
	if body["next_cursor"] != nil {
		t.Fatalf("next_cursor must be null: %v", body["next_cursor"])
	}
}

func TestListReleaseBatchesUnknownEnvironment(t *testing.T) {
	router := newTestRouter(t)
	seedReleaseBatchFixture(t, router)
	recorder := doRequest(t, router, http.MethodGet,
		"/api/v1/release-batches?environment=ghost", "")
	wantError(t, recorder, http.StatusNotFound, store.CodeEnvironmentNotFoundV1)
}

func TestListReleaseBatchesInvalidQueries(t *testing.T) {
	router := newTestRouter(t)
	seedReleaseBatchFixture(t, router)
	for _, target := range []string{
		"/api/v1/release-batches?gate_status=approved",
		"/api/v1/release-batches?from=2026-01-01",
		"/api/v1/release-batches?from=tuesday",
		"/api/v1/release-batches?from=2026-10-02T00:00:00Z&to=2026-10-01T00:00:00Z",
		"/api/v1/release-batches?limit=0",
		"/api/v1/release-batches?limit=101",
		"/api/v1/release-batches?limit=abc",
		"/api/v1/release-batches?limit=",
		"/api/v1/release-batches?environment=",
		"/api/v1/release-batches?gate_status=",
		"/api/v1/release-batches?cursor=",
		"/api/v1/release-batches?cursor=not-a-cursor",
	} {
		recorder := doRequest(t, router, http.MethodGet, target, "")
		wantError(t, recorder, http.StatusBadRequest, store.CodeBatchQueryInvalidV1)
	}
}

func TestListReleaseBatchesCursorRejectsRebindingAndTampering(t *testing.T) {
	router := newTestRouter(t)
	seedReleaseBatchFixture(t, router)

	first := doRequest(t, router, http.MethodGet, "/api/v1/release-batches?gate_status=allowed&limit=1", "")
	cursor, _ := decodeBatchesBody(t, first)["next_cursor"].(string)
	if cursor == "" {
		t.Fatal("expected a cursor from the first page")
	}

	// Filter mismatch: cursor was minted for gate_status=allowed.
	rebound := doRequest(t, router, http.MethodGet,
		"/api/v1/release-batches?gate_status=blocked&limit=1&cursor="+cursor, "")
	wantError(t, rebound, http.StatusBadRequest, store.CodeBatchQueryInvalidV1)

	// Tamper with the signed payload.
	parts := strings.Split(cursor, ".")
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	var marker map[string]any
	if err := json.Unmarshal(payload, &marker); err != nil {
		t.Fatal(err)
	}
	marker["batch_id"] = "b-999"
	tampered, _ := json.Marshal(marker)
	forged := base64.RawURLEncoding.EncodeToString(tampered) + "." + parts[1]
	forgedResponse := doRequest(t, router, http.MethodGet,
		"/api/v1/release-batches?gate_status=allowed&limit=1&cursor="+forged, "")
	wantError(t, forgedResponse, http.StatusBadRequest, store.CodeBatchQueryInvalidV1)

	// Original cursor still walks correctly.
	good := doRequest(t, router, http.MethodGet,
		"/api/v1/release-batches?gate_status=allowed&limit=1&cursor="+cursor, "")
	if good.Code != http.StatusOK {
		t.Fatalf("original cursor status = %d (body %s)", good.Code, good.Body.String())
	}
}

func TestGetReleaseBatchDetail(t *testing.T) {
	router := newTestRouter(t)
	seedReleaseBatchFixture(t, router)

	recorder := doRequest(t, router, http.MethodGet, "/api/v1/release-batches/b-2", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	body := decodeBody(t, recorder)
	if len(body) != 1 {
		t.Fatalf("detail response must contain only batch: %s", recorder.Body.String())
	}
	batch, ok := body["batch"].(map[string]any)
	if !ok {
		t.Fatalf("batch missing: %s", recorder.Body.String())
	}
	if batch["batch_id"] != "b-2" {
		t.Fatalf("batch_id = %v", batch["batch_id"])
	}
	if batch["release_count"].(float64) != 2 {
		t.Fatalf("release_count = %v", batch["release_count"])
	}
	if batch["change_count"].(float64) != 2 {
		t.Fatalf("change_count = %v", batch["change_count"])
	}
	counts, _ := batch["gate_counts"].(map[string]any)
	if counts["allowed"].(float64) != 1 || counts["blocked"].(float64) != 0 || counts["pending"].(float64) != 1 {
		t.Fatalf("gate_counts = %v", counts)
	}
	releases, _ := batch["releases"].([]any)
	if len(releases) != 2 {
		t.Fatalf("releases count = %d, want 2: %v", len(releases), releases)
	}
	for _, raw := range releases {
		release := raw.(map[string]any)
		if release["batch_id"] != "b-2" {
			t.Fatalf("release batch_id = %v", release["batch_id"])
		}
		if _, ok := release["id"].(string); !ok {
			t.Fatalf("release id missing: %v", release)
		}
		if _, ok := release["changes"].([]any); !ok {
			t.Fatalf("release changes must be a full array: %v", release)
		}
	}
	// recorded_at ascending; when both writes share a second, the spec
	// orders the tie by internal id descending (the later write first).
	firstRelease := releases[0].(map[string]any)
	secondRelease := releases[1].(map[string]any)
	if firstRelease["recorded_at"] == secondRelease["recorded_at"] {
		if firstRelease["environment"] != "prod" || secondRelease["environment"] != "dev" {
			t.Fatalf("same-second ties must order id descending: %v then %v",
				firstRelease["environment"], secondRelease["environment"])
		}
	} else if firstRelease["environment"] != "dev" || secondRelease["environment"] != "prod" {
		t.Fatalf("releases must be oldest first: %v then %v",
			firstRelease["environment"], secondRelease["environment"])
	}
	for _, key := range []string{"release_count", "environments", "change_count", "gate_counts", "last_recorded_at"} {
		if _, ok := batch[key]; !ok {
			t.Fatalf("batch detail missing summary key %s: %v", key, batch)
		}
	}
}

func TestGetReleaseBatchSameSecondTieBreaksByIDDescending(t *testing.T) {
	router := newTestRouter(t)
	for _, env := range []string{"qa", "uat"} {
		registerEnvironmentOK(t, router, env)
	}
	// Two distinct environments so both writes succeed; order in the fixture
	// is qa then uat within one server-second.
	first := createRecordOK(t, router, batchRecordBody("b-tie", "qa", "1.0.0", "allowed", "snap:0", "qa"))
	second := createRecordOK(t, router, batchRecordBody("b-tie", "uat", "1.0.0", "allowed", "snap:0", "uat"))

	recorder := doRequest(t, router, http.MethodGet, "/api/v1/release-batches/b-tie", "")
	batch := decodeBody(t, recorder)["batch"].(map[string]any)
	releases := batch["releases"].([]any)
	if len(releases) != 2 {
		t.Fatalf("releases count = %d", len(releases))
	}
	gotFirst := releases[0].(map[string]any)
	gotSecond := releases[1].(map[string]any)
	if gotFirst["recorded_at"] == gotSecond["recorded_at"] {
		if gotFirst["id"] != second["id"] || gotSecond["id"] != first["id"] {
			t.Fatalf("same-second ties must order by id descending: %v then %v",
				gotFirst["id"], gotSecond["id"])
		}
	} else if gotFirst["id"] != first["id"] || gotSecond["id"] != second["id"] {
		t.Fatalf("cross-second order must be chronological: %v then %v",
			gotFirst["id"], gotSecond["id"])
	}
}

func TestGetReleaseBatchNotFound(t *testing.T) {
	router := newTestRouter(t)
	seedReleaseBatchFixture(t, router)
	recorder := doRequest(t, router, http.MethodGet, "/api/v1/release-batches/does-not-exist", "")
	wantError(t, recorder, http.StatusNotFound, store.CodeReleaseBatchNotFoundV1)
}

func TestReleaseBatchesStorageUnavailable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := store.Open(filepath.Join(t.TempDir(), "batch.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	router := gin.New()
	Register(router, Dependencies{Store: db})
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	listRecorder := doRequest(t, router, http.MethodGet, "/api/v1/release-batches", "")
	wantError(t, listRecorder, http.StatusServiceUnavailable, store.CodeStorageUnavailable)
	detailRecorder := doRequest(t, router, http.MethodGet, "/api/v1/release-batches/b-1", "")
	wantError(t, detailRecorder, http.StatusServiceUnavailable, store.CodeStorageUnavailable)
}
