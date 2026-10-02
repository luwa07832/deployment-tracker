package v1

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/luwa07832/deployment-tracker/internal/store"
)

func newChangeTestRouter(t *testing.T) (*gin.Engine, *store.Store) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := store.Open(filepath.Join(t.TempDir(), "change-entries.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	engine := gin.New()
	Register(engine, Dependencies{Store: db})
	return engine, db
}

func entryMap(sequence int, category, title, description string) map[string]any {
	return map[string]any{
		"sequence":    sequence,
		"category":    category,
		"title":       title,
		"description": description,
	}
}

func changeRecordBody(environment, version, gate, rollback, batch string, entries ...map[string]any) string {
	record := map[string]any{
		"environment":    environment,
		"version":        version,
		"gate_status":    gate,
		"rollback_point": rollback,
		"changes":        entries,
	}
	if batch != "" {
		record["batch_id"] = batch
	}
	raw, _ := json.Marshal(record)
	return string(raw)
}

func pinRecordedAt(t *testing.T, db *store.Store, environment, version, recordedAt string) {
	t.Helper()
	records, err := db.ListReleaseRecords(store.ReleaseRecordFilter{
		Environment: environment, Version: version,
	})
	if err != nil || len(records) != 1 {
		t.Fatalf("locate %s %s: %+v %v", environment, version, records, err)
	}
	if err := db.SetRecordedAtForTest(records[0].ID, recordedAt); err != nil {
		t.Fatalf("pin recorded_at: %v", err)
	}
}

// seedChangeFixtures writes three release records with deterministic
// recorded_at timestamps and structured change entries.
func seedChangeFixtures(t *testing.T, router *gin.Engine, db *store.Store) {
	t.Helper()
	registerEnvironmentOK(t, router, "prod")
	registerEnvironmentOK(t, router, "dev")
	createRecordOK(t, router, changeRecordBody("prod", "1.0.0", "allowed", "rb:1", "",
		entryMap(2, "fix", "zeta", "old zeta"), entryMap(1, "feature", "alpha", "old alpha")))
	pinRecordedAt(t, db, "prod", "1.0.0", "2026-10-01T08:00:00Z")
	createRecordOK(t, router, changeRecordBody("prod", "1.1.0", "blocked", "rb:2", "b-1",
		entryMap(1, "feature", "beta", "blocked beta")))
	pinRecordedAt(t, db, "prod", "1.1.0", "2026-10-01T09:00:00Z")
	createRecordOK(t, router, changeRecordBody("dev", "1.0.0", "pending", "rb:3", "",
		entryMap(1, "fix", "alpha", "dev alpha")))
	pinRecordedAt(t, db, "dev", "1.0.0", "2026-10-02T08:00:00Z")
}

func decodeChangesResponse(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	body := decodeBody(t, recorder)
	for _, key := range []string{"changes", "next_cursor"} {
		if _, ok := body[key]; !ok {
			t.Fatalf("response missing %s: %s", key, recorder.Body.String())
		}
	}
	if len(body) != 2 {
		t.Fatalf("response must contain only changes and next_cursor: %s", recorder.Body.String())
	}
	return body
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func changeTitles(t *testing.T, body map[string]any) []string {
	t.Helper()
	rawChanges, ok := body["changes"].([]any)
	if !ok {
		t.Fatalf("changes is not an array: %v", body["changes"])
	}
	titles := make([]string, 0, len(rawChanges))
	for _, raw := range rawChanges {
		entry, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("change is not an object: %v", raw)
		}
		titles = append(titles, fmt.Sprintf("%v/%v#%v", entry["environment"], entry["version"], entry["title"]))
	}
	return titles
}

func TestChangeEntriesOrdersAndShapesEntries(t *testing.T) {
	router, db := newChangeTestRouter(t)
	seedChangeFixtures(t, router, db)

	recorder := doRequest(t, router, http.MethodGet, "/api/v1/change-entries", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", recorder.Code, recorder.Body.String())
	}
	body := decodeChangesResponse(t, recorder)
	if body["next_cursor"] != "" {
		t.Fatalf("single page must end with empty next_cursor")
	}
	want := []string{
		"dev/1.0.0#alpha",
		"prod/1.1.0#beta",
		"prod/1.0.0#alpha",
		"prod/1.0.0#zeta",
	}
	if got := changeTitles(t, body); !stringSlicesEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	entries := body["changes"].([]any)
	first := entries[0].(map[string]any)
	for key, value := range map[string]any{
		"sequence": float64(1), "category": "fix", "title": "alpha", "description": "dev alpha",
		"environment": "dev", "version": "1.0.0", "gate_status": "pending",
		"rollback_point": "rb:3", "recorded_at": "2026-10-02T08:00:00Z",
	} {
		if first[key] != value {
			t.Fatalf("first entry field %s = %v, want %v (full %v)", key, first[key], value, first)
		}
	}
	id, _ := first["id"].(string)
	if len(id) != 36 || id[:4] != "rel_" {
		t.Fatalf("id must be the owning release public id, got %v", first["id"])
	}
	if _, present := first["batch_id"]; present {
		t.Fatalf("batch_id must be omitted for unbatched releases, got %v", first["batch_id"])
	}
	beta := entries[1].(map[string]any)
	if beta["batch_id"] != "b-1" {
		t.Fatalf("batched release entry must expose batch_id: %v", beta)
	}
}

func TestChangeEntriesFiltersByAND(t *testing.T) {
	router, db := newChangeTestRouter(t)
	seedChangeFixtures(t, router, db)

	cases := []struct {
		name string
		path string
		want []string
	}{
		{"environment", "?environment=dev", []string{"dev/1.0.0#alpha"}},
		{"version", "?version=1.0.0", []string{"dev/1.0.0#alpha", "prod/1.0.0#alpha", "prod/1.0.0#zeta"}},
		{"batch_id", "?batch_id=b-1", []string{"prod/1.1.0#beta"}},
		{"category", "?category=fix", []string{"dev/1.0.0#alpha", "prod/1.0.0#zeta"}},
		{"title", "?title=alpha", []string{"dev/1.0.0#alpha", "prod/1.0.0#alpha"}},
		{"gate_status", "?gate_status=allowed", []string{"prod/1.0.0#alpha", "prod/1.0.0#zeta"}},
		{"from", "?from=2026-10-01T09:00:00Z", []string{"dev/1.0.0#alpha", "prod/1.1.0#beta"}},
		{"to", "?to=2026-10-01T08:00:00Z", []string{"prod/1.0.0#alpha", "prod/1.0.0#zeta"}},
		{"range", "?from=2026-10-01T08:30:00Z&to=2026-10-01T20:00:00Z", []string{"prod/1.1.0#beta"}},
		{"combined", "?environment=prod&version=1.0.0&category=feature&title=alpha&gate_status=allowed",
			[]string{"prod/1.0.0#alpha"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := doRequest(t, router, http.MethodGet, "/api/v1/change-entries"+tc.path, "")
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, body %s", recorder.Code, recorder.Body.String())
			}
			body := decodeChangesResponse(t, recorder)
			if got := changeTitles(t, body); !stringSlicesEqual(got, tc.want) {
				t.Fatalf("%s: got %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

func TestChangeEntriesEmptyResultIsEmptyArray(t *testing.T) {
	router, db := newChangeTestRouter(t)
	seedChangeFixtures(t, router, db)

	recorder := doRequest(t, router, http.MethodGet, "/api/v1/change-entries?title=missing", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	if got := recorder.Body.String(); got != `{"changes":[],"next_cursor":""}` {
		t.Fatalf("empty result body = %s", got)
	}
}

func TestChangeEntriesUnknownEnvironment(t *testing.T) {
	router, db := newChangeTestRouter(t)
	seedChangeFixtures(t, router, db)

	recorder := doRequest(t, router, http.MethodGet, "/api/v1/change-entries?environment=ghost", "")
	wantError(t, recorder, http.StatusNotFound, "ENVIRONMENT_NOT_FOUND")
}

func TestChangeEntriesValidationFailures(t *testing.T) {
	router, db := newChangeTestRouter(t)
	seedChangeFixtures(t, router, db)

	valid := "environment=prod&from=2026-10-01T08:00:00Z&to=2026-10-02T08:00:00Z&gate_status=allowed"
	cases := []struct {
		name string
		path string
	}{
		{"blank environment", "?environment=%20%20"},
		{"blank version", "?version=%09"},
		{"blank batch_id", "?batch_id="},
		{"blank category", "?category=%20"},
		{"blank title", "?title="},
		{"blank gate_status", "?gate_status="},
		{"blank from", "?from="},
		{"blank to", "?to=%20"},
		{"blank limit", "?limit=%20"},
		{"blank cursor", "?cursor="},
		{"limit zero", "?limit=0"},
		{"limit over max", "?limit=101"},
		{"limit not integer", "?limit=2.5"},
		{"limit not number", "?limit=abc"},
		{"limit negative", "?limit=-1"},
		{"bad gate status", "?gate_status=yes"},
		{"bad from format date", "?from=2026-10-01"},
		{"bad from format offset", "?from=2026-10-01T08:00:00+08:00"},
		{"bad from fractional", "?from=2026-10-01T08:00:00.5Z"},
		{"bad to format", "?to=2026-13-40T99:99:99Z"},
		{"from later than to", "?from=2026-10-03T00:00:00Z&to=2026-10-01T00:00:00Z"},
		{"truncated cursor", "?cursor=abc"},
		{"forged cursor", "?cursor=eyJhIjoxfQ.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := doRequest(t, router, http.MethodGet, "/api/v1/change-entries"+tc.path, "")
			wantError(t, recorder, http.StatusBadRequest, "INVALID_CHANGE_QUERY")
		})
	}
	// The same valid query is accepted, proving the validator is not over-eager.
	recorder := doRequest(t, router, http.MethodGet, "/api/v1/change-entries?"+valid, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("valid query rejected: %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestChangeEntriesValidatesBeforeEnvironmentExistence(t *testing.T) {
	router, _ := newChangeTestRouter(t)
	// No environments registered: bad limit on an unknown environment must
	// still surface the 400 query error rather than the 404.
	recorder := doRequest(t, router, http.MethodGet,
		"/api/v1/change-entries?environment=ghost&limit=nope", "")
	wantError(t, recorder, http.StatusBadRequest, "INVALID_CHANGE_QUERY")

	recorder = doRequest(t, router, http.MethodGet,
		"/api/v1/change-entries?environment=ghost&cursor=garbage", "")
	wantError(t, recorder, http.StatusBadRequest, "INVALID_CHANGE_QUERY")
}

func TestChangeEntriesPaginationWalksEveryEntryOnce(t *testing.T) {
	router, db := newChangeTestRouter(t)
	seedChangeFixtures(t, router, db)

	path := "/api/v1/change-entries?limit=2"
	var allTitles []string
	var cursorsUsed []string
	for page := 0; page < 10; page++ {
		recorder := doRequest(t, router, http.MethodGet, path, "")
		if recorder.Code != http.StatusOK {
			t.Fatalf("page %d status = %d, %s", page, recorder.Code, recorder.Body.String())
		}
		body := decodeChangesResponse(t, recorder)
		pageTitles := changeTitles(t, body)
		if len(pageTitles) > 2 {
			t.Fatalf("page %d returned %d entries", page, len(pageTitles))
		}
		allTitles = append(allTitles, pageTitles...)
		next, _ := body["next_cursor"].(string)
		if next == "" {
			break
		}
		cursorsUsed = append(cursorsUsed, next)
		path = "/api/v1/change-entries?limit=2&cursor=" + next
	}
	want := []string{
		"dev/1.0.0#alpha",
		"prod/1.1.0#beta",
		"prod/1.0.0#alpha",
		"prod/1.0.0#zeta",
	}
	if !stringSlicesEqual(allTitles, want) {
		t.Fatalf("walked titles = %v, want %v", allTitles, want)
	}
	if len(cursorsUsed) != 1 {
		t.Fatalf("four entries with limit 2 need exactly one cursor, got %d", len(cursorsUsed))
	}
}

func TestChangeEntriesCursorBindsToFilter(t *testing.T) {
	router, db := newChangeTestRouter(t)
	seedChangeFixtures(t, router, db)

	first := doRequest(t, router, http.MethodGet, "/api/v1/change-entries?limit=1&environment=prod", "")
	body := decodeChangesResponse(t, first)
	cursor, _ := body["next_cursor"].(string)
	if cursor == "" {
		t.Fatal("expected a next_cursor")
	}
	// Same conditions continue paging.
	same := doRequest(t, router, http.MethodGet,
		"/api/v1/change-entries?limit=1&environment=prod&cursor="+cursor, "")
	if same.Code != http.StatusOK {
		t.Fatalf("same filter cursor rejected: %d %s", same.Code, same.Body.String())
	}
	// Rebinding the cursor to different conditions is rejected.
	mismatches := []string{
		"/api/v1/change-entries?limit=1&environment=dev&cursor=" + cursor,
		"/api/v1/change-entries?limit=1&environment=prod&version=1.1.0&cursor=" + cursor,
		"/api/v1/change-entries?limit=1&environment=prod&gate_status=blocked&cursor=" + cursor,
		"/api/v1/change-entries?limit=1&environment=prod&from=2026-10-01T08:30:00Z&cursor=" + cursor,
		"/api/v1/change-entries?limit=1&cursor=" + cursor,
	}
	for _, path := range mismatches {
		recorder := doRequest(t, router, http.MethodGet, path, "")
		wantError(t, recorder, http.StatusBadRequest, "INVALID_CHANGE_QUERY")
	}
}

func TestChangeEntriesExcludesBaselineReleases(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := store.Open(filepath.Join(t.TempDir(), "baseline.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// Write a legacy deployment row directly, mirroring POST /releases storage.
	if _, err := db.EnsureEnvironment(&store.TrackedEnvironment{Environment: "prod"}); err != nil {
		t.Fatal(err)
	}
	createRecordOK(t, newV1RouterWith(t, db), changeRecordBody("prod", "1.0.0", "allowed", "rb", "",
		entryMap(1, "feature", "structured", "d")))
	if err := db.InsertRelease(&store.Release{
		Name: "legacy", Environment: "prod", Version: "2.0.0", Changes: []string{"legacy change"},
	}); err != nil {
		t.Fatal(err)
	}
	engine := newV1RouterWith(t, db)
	recorder := doRequest(t, engine, http.MethodGet, "/api/v1/change-entries", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d %s", recorder.Code, recorder.Body.String())
	}
	body := decodeChangesResponse(t, recorder)
	if got := changeTitles(t, body); !stringSlicesEqual(got, []string{"prod/1.0.0#structured"}) {
		t.Fatalf("baseline changes must never appear: %v", got)
	}
}

func newV1RouterWith(t *testing.T, db *store.Store) *gin.Engine {
	t.Helper()
	engine := gin.New()
	Register(engine, Dependencies{Store: db})
	return engine
}

func TestChangeEntriesCursorWalkIgnoresConcurrentInserts(t *testing.T) {
	router, db := newChangeTestRouter(t)
	seedChangeFixtures(t, router, db)

	first := doRequest(t, router, http.MethodGet, "/api/v1/change-entries?limit=2", "")
	body := decodeChangesResponse(t, first)
	cursor, _ := body["next_cursor"].(string)
	if cursor == "" {
		t.Fatal("expected a next_cursor")
	}
	seen := changeTitles(t, body)

	// A newer matching release lands while the client pages through.
	registerEnvironmentOK(t, router, "qa")
	createRecordOK(t, router, changeRecordBody("qa", "1.0.0", "allowed", "rb:9", "",
		entryMap(1, "feature", "just landed", "new after first page")))
	pinRecordedAt(t, db, "qa", "1.0.0", "2026-10-03T00:00:00Z")

	// Continuation pages must not surface the new entry: the initial snapshot
	// position is anchored to what existed before it.
	for page := 0; page < 10; page++ {
		recorder := doRequest(t, router, http.MethodGet, "/api/v1/change-entries?limit=2&cursor="+cursor, "")
		if recorder.Code != http.StatusOK {
			t.Fatalf("page status = %d %s", recorder.Code, recorder.Body.String())
		}
		pageBody := decodeChangesResponse(t, recorder)
		seen = append(seen, changeTitles(t, pageBody)...)
		cursor, _ = pageBody["next_cursor"].(string)
		if cursor == "" {
			break
		}
	}
	want := []string{
		"dev/1.0.0#alpha",
		"prod/1.1.0#beta",
		"prod/1.0.0#alpha",
		"prod/1.0.0#zeta",
	}
	if !stringSlicesEqual(seen, want) {
		t.Fatalf("cursor walk altered by concurrent insert: %v, want %v", seen, want)
	}

	// A fresh first request does observe the newly written entry first.
	fresh := doRequest(t, router, http.MethodGet, "/api/v1/change-entries?limit=1", "")
	freshBody := decodeChangesResponse(t, fresh)
	if got := changeTitles(t, freshBody); !stringSlicesEqual(got, []string{"qa/1.0.0#just landed"}) {
		t.Fatalf("fresh query must show new newest entry, got %v", got)
	}
}

func TestChangeEntriesStorageUnavailable(t *testing.T) {
	router, db := newChangeTestRouter(t)
	registerEnvironmentOK(t, router, "prod")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	recorder := doRequest(t, router, http.MethodGet, "/api/v1/change-entries?environment=prod", "")
	wantError(t, recorder, http.StatusServiceUnavailable, "storage_unavailable")
}
