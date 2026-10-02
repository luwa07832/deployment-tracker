package v1

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/luwa07832/deployment-tracker/internal/store"
)

func newTestRouterWithStore(t *testing.T) (*gin.Engine, *store.Store) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := store.Open(filepath.Join(t.TempDir(), "v1-atomic.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	engine := gin.New()
	Register(engine, Dependencies{Store: db})
	return engine, db
}

func concurrentRecordBody(version, gate, rollback string, seq int, batch string) string {
	body := recordBody("prod", version, gate, rollback,
		entry(1, "feature", "change-"+string(rune('a'+seq)), "description "+string(rune('a'+seq))))
	if batch != "" {
		// Insert the optional batch field right after the environment value.
		body = strings.Replace(body, `"environment":"prod"`, `"environment":"prod","batch_id":"`+batch+`"`, 1)
	}
	return body
}

// TestConcurrentPostsLeaveSingleEffectiveRecord launches simultaneous valid
// POSTs for the same environment and version with different batch ids and
// payloads. Exactly one returns 201 with the full record; every other request
// returns 409 RELEASE_ALREADY_EXISTS and leaves no readable residue.
func TestConcurrentPostsLeaveSingleEffectiveRecord(t *testing.T) {
	router, _ := newTestRouterWithStore(t)
	registerEnvironmentOK(t, router, "prod")
	registerEnvironmentOK(t, router, "staging")

	// Rollback targets must exist on both environments for release-comparison.
	createRecordOK(t, router, recordBody("prod", "1.0.0", "allowed", "1.0.0",
		entry(1, "feature", "base", "base")))
	stagingBase := `{"environment":"staging","version":"1.0.0","changes":[
		{"sequence":1,"category":"feature","title":"base","description":"base"}],
		"gate_status":"allowed","rollback_point":"1.0.0"}`
	createRecordOK(t, router, stagingBase)

	const total = 6
	bodies := make([]string, total)
	for i := range bodies {
		gate := []string{"allowed", "blocked", "pending"}[i%3]
		batch := ""
		if i%2 == 0 {
			batch = "batch-" + string(rune('a'+i))
		}
		bodies[i] = concurrentRecordBody("2.0.0", gate, "1.0.0", i, batch)
	}

	statuses := make([]int, total)
	payloads := make([]string, total)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			recorder := doRequest(t, router, http.MethodPost, "/api/v1/release-records", bodies[i])
			statuses[i] = recorder.Code
			payloads[i] = recorder.Body.String()
		}(i)
	}
	close(start)
	wg.Wait()

	created, conflicts := 0, 0
	var winnerID string
	for i, status := range statuses {
		switch status {
		case http.StatusCreated:
			created++
			record := mustDecode(t, payloads[i])["release_record"].(map[string]any)
			winnerID = record["id"].(string)
			if len(record["changes"].([]any)) != 1 || record["environment"] != "prod" ||
				record["version"] != "2.0.0" || record["rollback_point"] != "1.0.0" {
				t.Fatalf("winner record incomplete: %v", record)
			}
		case http.StatusConflict:
			errObj := mustDecode(t, payloads[i])["error"].(map[string]any)
			if errObj["code"] != store.CodeReleaseAlreadyExistsV1 {
				t.Fatalf("conflict body %s, want RELEASE_ALREADY_EXISTS", payloads[i])
			}
			conflicts++
		default:
			t.Fatalf("unexpected status %d body %s", status, payloads[i])
		}
	}
	if created != 1 || conflicts != total-1 {
		t.Fatalf("created = %d conflicts = %d, want 1 and %d", created, conflicts, total-1)
	}

	// The list endpoint exposes exactly one effective version.
	recorder := doRequest(t, router, http.MethodGet,
		"/api/v1/release-records?environment=prod&version=2.0.0", "")
	list := mustDecode(t, recorder.Body.String())["release_records"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["id"] != winnerID {
		t.Fatalf("parallel records visible through list: %v", list)
	}

	// Single-record fetch of the winner is complete and stable.
	first := doRequest(t, router, http.MethodGet, "/api/v1/release-records/"+winnerID, "")
	second := doRequest(t, router, http.MethodGet, "/api/v1/release-records/"+winnerID, "")
	if first.Code != http.StatusOK || second.Code != http.StatusOK ||
		first.Body.String() != second.Body.String() {
		t.Fatalf("winner fetch not stable: %d %s vs %d %s",
			first.Code, first.Body.String(), second.Code, second.Body.String())
	}

	// History contains the winner once in newest-first order.
	history := doRequest(t, router, http.MethodGet,
		"/api/v1/environments/prod/release-history?limit=10", "")
	releases := mustDecode(t, history.Body.String())["releases"].([]any)
	matches := 0
	for _, release := range releases {
		if release.(map[string]any)["version"] == "2.0.0" {
			matches++
			if release.(map[string]any)["id"] != winnerID {
				t.Fatalf("history carries a different parallel winner: %v", release)
			}
		}
	}
	if matches != 1 {
		t.Fatalf("history shows winner %d times, want 1: %s", matches, history.Body.String())
	}

	// Release comparison resolves a single effective 2.0.0 on prod.
	comparison := doRequest(t, router, http.MethodGet,
		"/api/v1/release-comparison?left=prod&left_version=2.0.0&right=staging&right_version=1.0.0", "")
	if comparison.Code != http.StatusOK {
		t.Fatalf("release-comparison status = %d body %s", comparison.Code, comparison.Body.String())
	}
	left := mustDecode(t, comparison.Body.String())["left"].(map[string]any)
	if left["id"] != winnerID {
		t.Fatalf("comparison left = %v, want %s", left, winnerID)
	}

	// Repeating the losing request later is still a stable conflict and never
	// creates a second row.
	recorder = doRequest(t, router, http.MethodPost, "/api/v1/release-records", bodies[1])
	wantError(t, recorder, http.StatusConflict, store.CodeReleaseAlreadyExistsV1)
	recorder = doRequest(t, router, http.MethodGet,
		"/api/v1/release-records?environment=prod&version=2.0.0", "")
	list = mustDecode(t, recorder.Body.String())["release_records"].([]any)
	if len(list) != 1 {
		t.Fatalf("conflicting post created a residue row: %v", list)
	}
}

// TestPostReturns503AndRetrySucceedsAfterAtomicFailure forces a storage
// failure while change entries are saved: the response is 503
// storage_unavailable with only the error key, nothing is queryable, and the
// identical retry returns 201.
func TestPostReturns503AndRetrySucceedsAfterAtomicFailure(t *testing.T) {
	router, db := newTestRouterWithStore(t)
	registerEnvironmentOK(t, router, "prod")

	body := recordBody("prod", "1.0.0", "allowed", "snapshot:0.9.0",
		entry(1, "feature", "change-a", "description a"))
	failed := false
	restore := store.SetReleaseInsertFaultHookForTest(func(*store.ReleaseRecord, int, int64) error {
		if !failed {
			failed = true
			return errFaultInjected
		}
		return nil
	})
	defer restore()

	recorder := doRequest(t, router, http.MethodPost, "/api/v1/release-records", body)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body %s)", recorder.Code, recorder.Body.String())
	}
	wantError(t, recorder, http.StatusServiceUnavailable, store.CodeStorageUnavailable)

	listed := doRequest(t, router, http.MethodGet,
		"/api/v1/release-records?environment=prod&version=1.0.0", "")
	if len(mustDecode(t, listed.Body.String())["release_records"].([]any)) != 0 {
		t.Fatalf("failed write left queryable records: %s", listed.Body.String())
	}

	_ = db
	createRecordOK(t, router, body)
}

var errFaultInjected = faultError("injected storage failure")

type faultError string

func (e faultError) Error() string { return string(e) }

func mustDecode(t *testing.T, body string) map[string]any {
	t.Helper()
	parsed := map[string]any{}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("decode body %q: %v", body, err)
	}
	return parsed
}
