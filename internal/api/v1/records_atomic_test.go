package v1

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/luwa07832/deployment-tracker/internal/store"

	_ "modernc.org/sqlite"
)

func newTestRouterAtPath(t *testing.T, path string) (*gin.Engine, *store.Store) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	engine := gin.New()
	Register(engine, Dependencies{Store: db})
	return engine, db
}

func releaseBody(batchID, gate, rollback string, sequences ...int) string {
	parts := make([]string, 0, len(sequences))
	for _, sequence := range sequences {
		parts = append(parts, fmt.Sprintf(
			`{"sequence":%d,"category":"feature","title":"change %d","description":"description %d"}`,
			sequence, sequence, sequence))
	}
	batch := ""
	if batchID != "" {
		batch = fmt.Sprintf(`,"batch_id":%q`, batchID)
	}
	return fmt.Sprintf(`{"environment":"prod","version":"4.0.0"%s,"changes":[%s],"gate_status":%q,"rollback_point":%q}`,
		batch, strings.Join(parts, ","), gate, rollback)
}

func expectErrorBody(t *testing.T, body, code string) error {
	t.Helper()
	var envelope map[string]any
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		return fmt.Errorf("decode %q: %w", body, err)
	}
	errorObject, ok := envelope["error"].(map[string]any)
	if !ok || errorObject["code"] != code {
		return fmt.Errorf("body %s, want error code %s", body, code)
	}
	if len(envelope) != 1 {
		return fmt.Errorf("error body must contain only the error key: %s", body)
	}
	return nil
}

// assertSingleEffectiveRelease checks every read surface that can expose a
// parallel effective version: filtered list, get by id and history.
func assertSingleEffectiveRelease(t *testing.T, router *gin.Engine, id string) {
	t.Helper()
	recorder := doRequest(t, router, http.MethodGet,
		"/api/v1/release-records?environment=prod&version=4.0.0", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("list status = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	list := decodeBody(t, recorder)["release_records"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["id"] != id {
		t.Fatalf("list must contain the single winner: %v", list)
	}
	recorder = doRequest(t, router, http.MethodGet, "/api/v1/release-records/"+id, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("get status = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	got := decodeBody(t, recorder)["release_record"].(map[string]any)
	if got["id"] != id || len(got["changes"].([]any)) != 2 {
		t.Fatalf("get = %v, want the complete winning record", got)
	}
}

func TestConcurrentSubmissionsSameEnvironmentVersionProduceOneRecord(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "prod")
	const submitters = 20

	bodies := make([]string, submitters)
	for i := range bodies {
		// Distinct batch ids and payloads must still serialize to one row.
		bodies[i] = releaseBody(fmt.Sprintf("batch-%d", i),
			[]string{"allowed", "blocked", "pending"}[i%3],
			fmt.Sprintf("rb:%d.0.0", i%2), 1, 2)
	}

	statuses := make([]int, submitters)
	responseBodies := make([]string, submitters)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < submitters; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			recorder := doRequest(t, router, http.MethodPost, "/api/v1/release-records", bodies[i])
			statuses[i] = recorder.Code
			responseBodies[i] = recorder.Body.String()
		}(i)
	}
	close(start)
	wg.Wait()

	created, conflicts := 0, 0
	var winner map[string]any
	for i, status := range statuses {
		switch status {
		case http.StatusCreated:
			created++
			winner = decodeBodyString(t, responseBodies[i])["release_record"].(map[string]any)
		case http.StatusConflict:
			conflicts++
			if err := expectErrorBody(t, responseBodies[i], store.CodeReleaseAlreadyExistsV1); err != nil {
				t.Fatalf("conflict body: %v", err)
			}
		default:
			t.Fatalf("submitter %d status = %d (body %s)", i, status, responseBodies[i])
		}
	}
	if created != 1 || conflicts != submitters-1 {
		t.Fatalf("created = %d, conflicts = %d, want 1 and %d", created, conflicts, submitters-1)
	}
	if winner["recorded_at"] == "" || winner["batch_id"] == "" {
		t.Fatalf("winner incomplete: %v", winner)
	}
	assertSingleEffectiveRelease(t, router, winner["id"].(string))

	// Repeated reads stay stable and never reveal the losing submissions.
	for i := 0; i < 3; i++ {
		assertSingleEffectiveRelease(t, router, winner["id"].(string))
	}
}

func decodeBodyString(t *testing.T, body string) map[string]any {
	t.Helper()
	var envelope map[string]any
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatalf("decode body %q: %v", body, err)
	}
	return envelope
}

func TestFailedSubmissionReturns503LeavesNothingAndRetriesCleanly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "atomic.db")
	router, _ := newTestRouterAtPath(t, path)
	registerEnvironmentOK(t, router, "prod")

	// Force the second change insert to fail inside the write transaction.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	t.Cleanup(func() { raw.Close() })
	if _, err := raw.Exec(`
CREATE TRIGGER fail_change_entry
AFTER INSERT ON release_change_entries
BEGIN
  SELECT CASE WHEN NEW.sequence_no = 2
    THEN RAISE(ABORT, 'simulated storage failure')
  END;
END;`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	recorder := doRequest(t, router, http.MethodPost, "/api/v1/release-records",
		releaseBody("batch-1", "allowed", "rb:3.0.0", 1, 2))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body %s)", recorder.Code, recorder.Body.String())
	}
	if err := expectErrorBody(t, recorder.Body.String(), store.CodeStorageUnavailable); err != nil {
		t.Fatalf("503 body: %v", err)
	}

	// No queryable record remains through the list or history endpoints.
	list := decodeBody(t, doRequest(t, router, http.MethodGet,
		"/api/v1/release-records?environment=prod&version=4.0.0", ""))["release_records"].([]any)
	if len(list) != 0 {
		t.Fatalf("half-written record is queryable: %v", list)
	}
	history := decodeBody(t, doRequest(t, router, http.MethodGet,
		"/api/v1/environments/prod/release-history", ""))["releases"].([]any)
	if len(history) != 0 {
		t.Fatalf("half-written record appears in history: %v", history)
	}
	var masters, entries int
	if err := raw.QueryRow(`SELECT count(1) FROM release_records`).Scan(&masters); err != nil {
		t.Fatalf("count masters: %v", err)
	}
	if err := raw.QueryRow(`SELECT count(1) FROM release_change_entries`).Scan(&entries); err != nil {
		t.Fatalf("count entries: %v", err)
	}
	if masters != 0 || entries != 0 {
		t.Fatalf("residue after failure: %d master rows, %d change entries", masters, entries)
	}

	// Retrying the identical fields and changes once storage recovers creates
	// the record instead of colliding with the half-written attempt.
	if _, err := raw.Exec(`DROP TRIGGER fail_change_entry`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}
	record := createRecordOK(t, router, releaseBody("batch-1", "allowed", "rb:3.0.0", 1, 2))
	if record["batch_id"] != "batch-1" || len(record["changes"].([]any)) != 2 {
		t.Fatalf("retried record incomplete: %v", record)
	}
	assertSingleEffectiveRelease(t, router, record["id"].(string))

	// Repeating the identical submission now reports the stable 409 conflict.
	recorder = doRequest(t, router, http.MethodPost, "/api/v1/release-records",
		releaseBody("batch-1", "allowed", "rb:3.0.0", 1, 2))
	wantError(t, recorder, http.StatusConflict, store.CodeReleaseAlreadyExistsV1)
}

func TestCompleteRegistrationFlowAcrossReadEndpoints(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "prod")
	registerEnvironmentOK(t, router, "stage")

	// Each rollback point must resolve to a recorded release in its own
	// environment for release-comparison to succeed.
	createRecordOK(t, router, strings.ReplaceAll(releaseBody("", "allowed", "2.0.0", 1), `"4.0.0"`, `"2.0.0"`))
	prodTargetBody := strings.ReplaceAll(releaseBody("", "allowed", "2.0.0", 1), `"4.0.0"`, `"3.0.0"`)
	createRecordOK(t, router, prodTargetBody)
	stageTargetBody := strings.ReplaceAll(
		strings.ReplaceAll(releaseBody("", "allowed", "rb:none", 1), `"prod"`, `"stage"`),
		`"4.0.0"`, `"3.5.0"`)
	stageRollbackBody := strings.ReplaceAll(
		strings.ReplaceAll(releaseBody("", "allowed", "3.0.0", 1), `"prod"`, `"stage"`),
		`"4.0.0"`, `"3.0.0"`)
	createRecordOK(t, router, stageRollbackBody)
	stageTargetBody = strings.ReplaceAll(stageTargetBody, `"rb:none"`, `"3.0.0"`)
	stageTarget := createRecordOK(t, router, stageTargetBody)

	winner := createRecordOK(t, router, releaseBody("batch-x", "allowed", "3.0.0", 1, 2))
	loser := doRequest(t, router, http.MethodPost, "/api/v1/release-records",
		releaseBody("batch-y", "blocked", "3.5.0", 7))
	wantError(t, loser, http.StatusConflict, store.CodeReleaseAlreadyExistsV1)

	id := winner["id"].(string)
	assertSingleEffectiveRelease(t, router, id)
	history := decodeBody(t, doRequest(t, router, http.MethodGet,
		"/api/v1/environments/prod/release-history?limit=10", ""))["releases"].([]any)
	if len(history) != 3 || history[0].(map[string]any)["id"] != id {
		t.Fatalf("history = %v, want the winner first then the rollback targets", history)
	}
	comparison := doRequest(t, router, http.MethodGet,
		"/api/v1/release-comparison?left=prod&left_version=4.0.0&right=stage&right_version=3.5.0", "")
	if comparison.Code != http.StatusOK {
		t.Fatalf("comparison status = %d (body %s)", comparison.Code, comparison.Body.String())
	}
	payload := decodeBody(t, comparison)
	if payload["left"].(map[string]any)["id"] != id ||
		payload["right"].(map[string]any)["id"] != stageTarget["id"] {
		t.Fatalf("comparison sides do not resolve the single effective rows: %v", payload)
	}
}
