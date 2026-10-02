package v1

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/luwa07832/deployment-tracker/internal/store"
)

// changeRecordBody builds a v1 release-record body with the given entries.
func changeRecordBody(environment, version, gate, rollback string, entries ...string) string {
	body := `{"environment":"` + environment + `","version":"` + version + `","changes":[`
	for i, entry := range entries {
		if i > 0 {
			body += ","
		}
		body += entry
	}
	return body + `],"gate_status":"` + gate + `","rollback_point":"` + rollback + `"}`
}

func withBatch(body, batchID string) string {
	return strings.Replace(body, `"changes"`, `"batch_id":"`+batchID+`","changes"`, 1)
}

func getChangeEntriesOK(t *testing.T, router *gin.Engine, target string) ([]any, string) {
	t.Helper()
	recorder := doRequest(t, router, http.MethodGet, target, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET %s status = %d (body %s)", target, recorder.Code, recorder.Body.String())
	}
	body := decodeBody(t, recorder)
	return body["changes"].([]any), body["next_cursor"].(string)
}

func itemAt(t *testing.T, changes []any, index int) map[string]any {
	t.Helper()
	item, ok := changes[index].(map[string]any)
	if !ok {
		t.Fatalf("change %d = %v", index, changes[index])
	}
	return item
}

func seedChangeEntries(t *testing.T, router *gin.Engine) (prodID, stagingID string) {
	t.Helper()
	registerEnvironmentOK(t, router, "prod")
	registerEnvironmentOK(t, router, "staging")

	// Older record with a batched release.
	batched := withBatch(changeRecordBody("prod", "1.0.0", "blocked", "snap:0.9.0",
		entry(3, "fix", "zeta", "old fix"),
		entry(1, "feature", "alpha", "old feature"),
		entry(2, "feature", "beta", "middle feature"),
	), "b-1")
	older := createRecordOK(t, router, batched)

	// Newer unbatched record on a different environment.
	newer := createRecordOK(t, router, changeRecordBody("staging", "2.0.0", "allowed", "snap:1.9.0",
		entry(2, "chore", "cleanup", "tidy up"),
		entry(1, "feature", "login", "users log in"),
	))
	return newer["id"].(string), older["id"].(string)
}

func TestChangeEntriesOrderingAndView(t *testing.T) {
	router := newTestRouter(t)
	stagingID, prodID := seedChangeEntries(t, router)

	changes, nextCursor := getChangeEntriesOK(t, router, "/api/v1/change-entries")
	if nextCursor != "" {
		t.Fatalf("next_cursor = %q, want empty on the last page", nextCursor)
	}
	if len(changes) != 5 {
		t.Fatalf("len(changes) = %d, want 5: %v", len(changes), changes)
	}

	// Newest release first; its entries sorted by sequence then title.
	first := itemAt(t, changes, 0)
	if first["title"] != "login" || first["sequence"] != float64(1) {
		t.Fatalf("first = %v", first)
	}
	if first["id"] != stagingID || first["environment"] != "staging" ||
		first["version"] != "2.0.0" || first["gate_status"] != "allowed" ||
		first["rollback_point"] != "snap:1.9.0" || first["recorded_at"] == "" {
		t.Fatalf("first release fields = %v", first)
	}
	if _, present := first["batch_id"]; present {
		t.Fatalf("unbatched record must omit batch_id: %v", first)
	}
	if itemAt(t, changes, 1)["title"] != "cleanup" {
		t.Fatalf("second = %v", changes[1])
	}

	// Older batched release; same-sequence entries follow title ascending.
	third := itemAt(t, changes, 2)
	if third["title"] != "alpha" || third["sequence"] != float64(1) {
		t.Fatalf("third = %v", third)
	}
	if itemAt(t, changes, 3)["title"] != "beta" {
		t.Fatalf("fourth = %v", changes[3])
	}
	fifth := itemAt(t, changes, 4)
	if fifth["title"] != "zeta" || fifth["id"] != prodID {
		t.Fatalf("fifth = %v", fifth)
	}
	if fifth["batch_id"] != "b-1" || fifth["gate_status"] != "blocked" {
		t.Fatalf("batched release fields = %v", fifth)
	}
}

func TestChangeEntriesEmptyEnvelope(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "prod")
	recorder := doRequest(t, router, http.MethodGet, "/api/v1/change-entries?environment=prod", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	body := decodeBody(t, recorder)
	changes := body["changes"].([]any)
	if len(changes) != 0 {
		t.Fatalf("changes = %v, want []", changes)
	}
	if body["next_cursor"] != "" {
		t.Fatalf("next_cursor = %v", body["next_cursor"])
	}
	if len(body) != 2 {
		t.Fatalf("envelope must contain only changes and next_cursor: %v", body)
	}
}

func TestChangeEntriesANDFilters(t *testing.T) {
	router := newTestRouter(t)
	seedChangeEntries(t, router)

	changes, _ := getChangeEntriesOK(t, router,
		"/api/v1/change-entries?environment=prod&version=1.0.0&batch_id=b-1&gate_status=blocked&category=fix&title=zeta")
	if len(changes) != 1 || itemAt(t, changes, 0)["title"] != "zeta" {
		t.Fatalf("changes = %v", changes)
	}

	// AND semantics: one mismatching filter yields no hits, still 200.
	changes, _ = getChangeEntriesOK(t, router, "/api/v1/change-entries?environment=prod&title=login")
	if len(changes) != 0 {
		t.Fatalf("changes = %v, want none", changes)
	}

	// Unbatched-only semantics for a blank batch omission: batch filter
	// matches exactly the records carrying that batch id.
	changes, _ = getChangeEntriesOK(t, router, "/api/v1/change-entries?batch_id=b-1")
	if len(changes) != 3 {
		t.Fatalf("batched changes = %v", changes)
	}
}

func TestChangeEntriesTimeRange(t *testing.T) {
	router := newTestRouter(t)
	seedChangeEntries(t, router)

	// Everything recorded after 3000 is in the future: empty 200.
	changes, _ := getChangeEntriesOK(t, router, "/api/v1/change-entries?from=3000-01-01T00:00:00Z")
	if len(changes) != 0 {
		t.Fatalf("future from = %v", changes)
	}
	// Everything recorded before 2000 is in the past: empty 200.
	changes, _ = getChangeEntriesOK(t, router, "/api/v1/change-entries?to=2000-01-01T00:00:00Z")
	if len(changes) != 0 {
		t.Fatalf("ancient to = %v", changes)
	}
	// A window covering now returns all five entries.
	changes, _ = getChangeEntriesOK(t, router,
		"/api/v1/change-entries?from=2000-01-01T00:00:00Z&to=3000-01-01T00:00:00Z")
	if len(changes) != 5 {
		t.Fatalf("windowed changes = %v", changes)
	}
}

func TestChangeEntriesPagination(t *testing.T) {
	router := newTestRouter(t)
	seedChangeEntries(t, router)

	pageOne, cursorOne := getChangeEntriesOK(t, router, "/api/v1/change-entries?limit=2")
	if len(pageOne) != 2 || cursorOne == "" {
		t.Fatalf("page one = %v cursor %q", pageOne, cursorOne)
	}
	pageTwo, cursorTwo := getChangeEntriesOK(t, router, "/api/v1/change-entries?limit=2&cursor="+cursorOne)
	if len(pageTwo) != 2 || cursorTwo == "" {
		t.Fatalf("page two = %v cursor %q", pageTwo, cursorTwo)
	}
	pageThree, cursorThree := getChangeEntriesOK(t, router, "/api/v1/change-entries?limit=2&cursor="+cursorTwo)
	if len(pageThree) != 1 || cursorThree != "" {
		t.Fatalf("page three = %v cursor %q", pageThree, cursorThree)
	}

	seen := map[string]bool{}
	for _, page := range [][]any{pageOne, pageTwo, pageThree} {
		for _, raw := range page {
			item := raw.(map[string]any)
			key := item["id"].(string) + "|" + item["title"].(string) + "|" +
				strconv.FormatFloat(item["sequence"].(float64), 'f', -1, 64)
			if seen[key] {
				t.Fatalf("entry %s appeared on multiple pages", key)
			}
			seen[key] = true
		}
	}
	if len(seen) != 5 {
		t.Fatalf("paged through %d entries, want 5", len(seen))
	}

	// A larger limit on the same cursor position yields the next slice and
	// ends at the last page.
	pageWide, cursorWide := getChangeEntriesOK(t, router,
		"/api/v1/change-entries?limit=100&cursor="+cursorOne)
	if len(pageWide) != 3 || cursorWide != "" {
		t.Fatalf("wide page = %v cursor %q", pageWide, cursorWide)
	}
}

func TestChangeEntriesInvalidQueries(t *testing.T) {
	router := newTestRouter(t)
	seedChangeEntries(t, router)

	invalid := []string{
		"/api/v1/change-entries?environment=",
		"/api/v1/change-entries?version=%20%20",
		"/api/v1/change-entries?batch_id=%09",
		"/api/v1/change-entries?category=",
		"/api/v1/change-entries?title=",
		"/api/v1/change-entries?gate_status=",
		"/api/v1/change-entries?from=%20",
		"/api/v1/change-entries?to=",
		"/api/v1/change-entries?limit=",
		"/api/v1/change-entries?cursor=%20",
		"/api/v1/change-entries?limit=0",
		"/api/v1/change-entries?limit=101",
		"/api/v1/change-entries?limit=-1",
		"/api/v1/change-entries?limit=abc",
		"/api/v1/change-entries?limit=1.5",
		"/api/v1/change-entries?gate_status=shipped",
		"/api/v1/change-entries?from=2026-10-01",
		"/api/v1/change-entries?from=2026-10-01T08:00:00",
		"/api/v1/change-entries?from=2026-10-01T08:00:00+08:00",
		"/api/v1/change-entries?from=2026-10-01T08:00:00.000Z",
		"/api/v1/change-entries?from=2026-13-01T08:00:00Z",
		"/api/v1/change-entries?to=not-a-time",
		"/api/v1/change-entries?from=2026-10-02T00:00:00Z&to=2026-10-01T00:00:00Z",
		"/api/v1/change-entries?cursor=not-base64%21%21",
		"/api/v1/change-entries?cursor=eyJub3QiOiJjdXJzb3IifQ",
	}
	for _, target := range invalid {
		recorder := doRequest(t, router, http.MethodGet, target, "")
		wantError(t, recorder, http.StatusBadRequest, store.CodeInvalidChangeQueryV1)
	}
}

func TestChangeEntriesValidationBeforeEnvironment(t *testing.T) {
	router := newTestRouter(t)
	// No environment registered: an invalid query still reports 400 first.
	recorder := doRequest(t, router, http.MethodGet,
		"/api/v1/change-entries?environment=missing&limit=nope", "")
	wantError(t, recorder, http.StatusBadRequest, store.CodeInvalidChangeQueryV1)

	recorder = doRequest(t, router, http.MethodGet,
		"/api/v1/change-entries?environment=missing", "")
	wantError(t, recorder, http.StatusNotFound, store.CodeEnvironmentNotFoundV1)
}

func TestChangeEntriesCursorBoundToFilter(t *testing.T) {
	router := newTestRouter(t)
	seedChangeEntries(t, router)

	_, cursor := getChangeEntriesOK(t, router, "/api/v1/change-entries?environment=prod&limit=2")

	mismatches := []string{
		"/api/v1/change-entries?environment=staging&limit=2&cursor=",
		"/api/v1/change-entries?environment=prod&version=1.0.0&limit=2&cursor=",
		"/api/v1/change-entries?environment=prod&batch_id=b-1&limit=2&cursor=",
		"/api/v1/change-entries?environment=prod&category=fix&limit=2&cursor=",
		"/api/v1/change-entries?environment=prod&title=zeta&limit=2&cursor=",
		"/api/v1/change-entries?environment=prod&gate_status=blocked&limit=2&cursor=",
		"/api/v1/change-entries?environment=prod&from=2000-01-01T00:00:00Z&limit=2&cursor=",
		"/api/v1/change-entries?environment=prod&to=3000-01-01T00:00:00Z&limit=2&cursor=",
	}
	for _, target := range mismatches {
		recorder := doRequest(t, router, http.MethodGet, target+cursor, "")
		wantError(t, recorder, http.StatusBadRequest, store.CodeInvalidChangeQueryV1)
	}

	// The same filter accepts the cursor and returns the remainder.
	changes, nextCursor := getChangeEntriesOK(t, router,
		"/api/v1/change-entries?environment=prod&limit=2&cursor="+cursor)
	if len(changes) != 1 || nextCursor != "" {
		t.Fatalf("remainder = %v cursor %q", changes, nextCursor)
	}

	// limit is not part of the binding: another page size with the same
	// filter must keep working.
	changes, _ = getChangeEntriesOK(t, router,
		"/api/v1/change-entries?environment=prod&limit=5&cursor="+cursor)
	if len(changes) != 1 {
		t.Fatalf("rebound limit changes = %v", changes)
	}

	// Truncating the opaque token is rejected.
	recorder := doRequest(t, router, http.MethodGet,
		"/api/v1/change-entries?environment=prod&cursor="+cursor[:len(cursor)-2], "")
	wantError(t, recorder, http.StatusBadRequest, store.CodeInvalidChangeQueryV1)
}

func TestChangeEntriesExcludesBaselineReleases(t *testing.T) {
	router, db := newTestRouterWithStore(t)
	registerEnvironmentOK(t, router, "prod")
	gate := "allowed"
	rollback := "snap:0.1.0"
	if err := db.InsertRelease(&store.Release{
		Environment:   "prod",
		Version:       "9.9.9",
		Changes:       []string{"legacy string change"},
		GateStatus:    &gate,
		RollbackPoint: &rollback,
	}); err != nil {
		t.Fatalf("insert legacy release: %v", err)
	}
	createRecordOK(t, router, validRecordBody)

	changes, _ := getChangeEntriesOK(t, router, "/api/v1/change-entries")
	if len(changes) != 2 {
		t.Fatalf("changes = %v, want only the two structured v1 entries", changes)
	}
	for _, raw := range changes {
		item := raw.(map[string]any)
		if item["version"] == "9.9.9" {
			t.Fatalf("baseline release leaked into change entries: %v", item)
		}
	}
}

func TestChangeEntriesStorageUnavailable(t *testing.T) {
	router, db := newTestRouterWithStore(t)
	registerEnvironmentOK(t, router, "prod")
	createRecordOK(t, router, validRecordBody)
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	recorder := doRequest(t, router, http.MethodGet, "/api/v1/change-entries", "")
	wantError(t, recorder, http.StatusServiceUnavailable, store.CodeStorageUnavailable)
}

func TestChangeEntriesDefaultLimitCapsAtTwenty(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "prod")
	var entries []string
	for i := 1; i <= 25; i++ {
		entries = append(entries, entry(i, "feature", "change-"+strconv.Itoa(i), "desc"))
	}
	createRecordOK(t, router, changeRecordBody("prod", "1.0.0", "allowed", "snap:0", entries...))

	changes, cursor := getChangeEntriesOK(t, router, "/api/v1/change-entries")
	if len(changes) != 20 || cursor == "" {
		t.Fatalf("default page len = %d cursor %q", len(changes), cursor)
	}
	// Within one release the sequence order is ascending despite the
	// descending release order.
	if itemAt(t, changes, 0)["title"] != "change-1" {
		t.Fatalf("first = %v", changes[0])
	}
	changes, cursor = getChangeEntriesOK(t, router, "/api/v1/change-entries?cursor="+cursor)
	if len(changes) != 5 || cursor != "" {
		t.Fatalf("second page len = %d cursor %q", len(changes), cursor)
	}
}

func TestChangeEntriesLimitBoundaryAccepted(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "prod")
	createRecordOK(t, router, changeRecordBody("prod", "1.0.0", "allowed", "snap:0",
		entry(1, "feature", "only", "desc")))

	for _, raw := range []string{"1", "100"} {
		recorder := doRequest(t, router, http.MethodGet, "/api/v1/change-entries?limit="+raw, "")
		if recorder.Code != http.StatusOK {
			t.Fatalf("limit %s status = %d (body %s)", raw, recorder.Code, recorder.Body.String())
		}
	}
}
