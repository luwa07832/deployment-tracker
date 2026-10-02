package v1

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/luwa07832/deployment-tracker/internal/store"
)

// newDuplicateTitleRouter returns a router backed by a fresh database that
// tests can seed with pre-constraint historical records containing repeated
// change titles directly through the store.
func newDuplicateTitleRouter(t *testing.T) (*gin.Engine, *store.Store) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := store.Open(filepath.Join(t.TempDir(), "v1.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	engine := gin.New()
	Register(engine, Dependencies{Store: db})
	return engine, db
}

func registerScratchEnvironment(t *testing.T, db *store.Store, key string) {
	t.Helper()
	if _, err := db.EnsureEnvironment(&store.TrackedEnvironment{
		Environment: key, DisplayName: key + " display",
	}); err != nil {
		t.Fatalf("register environment: %v", err)
	}
}

func insertScratchRecord(t *testing.T, db *store.Store, batch, env, version, gate, point string, changes []store.ChangeEntry) string {
	t.Helper()
	record := &store.ReleaseRecord{
		Environment:   env,
		Version:       version,
		BatchID:       batch,
		GateStatus:    gate,
		RollbackPoint: point,
		Changes:       changes,
	}
	if err := db.InsertReleaseRecord(record); err != nil {
		t.Fatalf("insert record: %v", err)
	}
	return record.PublicID
}

func dupEntry(sequence int, description string) store.ChangeEntry {
	return store.ChangeEntry{Sequence: sequence, Category: "feature", Title: "dup", Description: description}
}

func TestCreateRecordRejectsDuplicateTitlesAtomically(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "prod")
	body := `{
	  "environment": "prod", "version": "1.0.0",
	  "changes": [
	    {"sequence": 1, "category": "feature", "title": "  shared title  ", "description": "a"},
	    {"sequence": 2, "category": "fix", "title": "shared title", "description": "b"}
	  ],
	  "gate_status": "allowed", "rollback_point": "0.9.0"
	}`
	recorder := doRequest(t, router, http.MethodPost, "/api/v1/release-records", body)
	wantError(t, recorder, http.StatusUnprocessableEntity, store.CodeReleaseValidationV1)

	// Neither the main record nor any change entry may survive the rejected
	// write: listing records for the environment stays empty.
	list := doRequest(t, router, http.MethodGet,
		"/api/v1/release-records?environment=prod&limit=10", "")
	var listed struct {
		Records []map[string]any `json:"release_records"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listed.Records) != 0 {
		t.Fatalf("rejected submission wrote records: %v", listed.Records)
	}
}

func TestHistoricalDuplicateTitlesStayReadable(t *testing.T) {
	router, db := newDuplicateTitleRouter(t)
	registerScratchEnvironment(t, db, "prod")
	changes := []store.ChangeEntry{
		dupEntry(1, "one"),
		dupEntry(2, "two"),
		{Sequence: 3, Category: "fix", Title: "solo", Description: "s"},
	}
	publicID := insertScratchRecord(t, db, "", "prod", "1.0.0", "allowed", "0.9.0", changes)
	recorder := doRequest(t, router, http.MethodGet, "/api/v1/release-records/"+publicID, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("get record %d: %s", recorder.Code, recorder.Body.String())
	}
	var resp struct {
		Record struct {
			Changes []struct {
				Sequence int    `json:"sequence"`
				Title    string `json:"title"`
			} `json:"changes"`
		} `json:"release_record"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode record: %v", err)
	}
	if len(resp.Record.Changes) != 3 {
		t.Fatalf("changes = %v, want all three original entries", resp.Record.Changes)
	}
	for i, entry := range resp.Record.Changes {
		if entry.Sequence != i+1 {
			t.Fatalf("entry %d sequence = %d, want original order", i, entry.Sequence)
		}
	}
}

// duplicate comparison seed: staging holds the title twice plus "solo", prod
// holds it once plus "extra" and "solo". Pairing yields one changed, one
// missing, one added and one unchanged entry.
func seedDuplicateComparison(t *testing.T, router *gin.Engine, db *store.Store, batch string) {
	for _, envName := range []string{"staging", "prod"} {
		registerScratchEnvironment(t, db, envName)
		insertScratchRecord(t, db, batch, envName, "0.9.0", "allowed", "0.8.0",
			[]store.ChangeEntry{{Sequence: 1, Category: "base", Title: "base", Description: "b"}})
	}
	insertScratchRecord(t, db, batch, "staging", "1.0.0", "allowed", "0.9.0", []store.ChangeEntry{
		dupEntry(1, "one"), dupEntry(2, "two"),
		{Sequence: 3, Category: "fix", Title: "solo", Description: "s"},
	})
	insertScratchRecord(t, db, batch, "prod", "1.0.0", "allowed", "0.9.0", []store.ChangeEntry{
		dupEntry(1, "a"),
		{Sequence: 2, Category: "feature", Title: "extra", Description: "e"},
		{Sequence: 3, Category: "fix", Title: "solo", Description: "s"},
	})
}

func TestCompareComparesEveryRepeatedTitleEntry(t *testing.T) {
	router, db := newDuplicateTitleRouter(t)
	seedDuplicateComparison(t, router, db, "")
	recorder := doRequest(t, router, http.MethodGet,
		"/api/v1/compare?left=staging&right=prod&version=1.0.0", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("compare %d: %s", recorder.Code, recorder.Body.String())
	}
	var resp struct {
		VersionDiffs []struct {
			Added   []map[string]any `json:"added_changes"`
			Removed []map[string]any `json:"removed_changes"`
			Changed []map[string]any `json:"changed_changes"`
		} `json:"version_diffs"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode compare: %v", err)
	}
	if len(resp.VersionDiffs) != 1 {
		t.Fatalf("version_diffs = %v, want one", resp.VersionDiffs)
	}
	diff := resp.VersionDiffs[0]
	if len(diff.Added) != 1 || diff.Added[0]["title"] != "extra" {
		t.Fatalf("added = %v", diff.Added)
	}
	if len(diff.Removed) != 1 || diff.Removed[0]["title"] != "dup" {
		t.Fatalf("removed = %v", diff.Removed)
	}
	if len(diff.Changed) != 1 {
		t.Fatalf("changed = %v, want the single paired dup entry", diff.Changed)
	}
}

func TestReleaseComparisonSummarizesRepeatedTitles(t *testing.T) {
	router, db := newDuplicateTitleRouter(t)
	seedDuplicateComparison(t, router, db, "")
	recorder := doRequest(t, router, http.MethodGet,
		"/api/v1/release-comparison?left=staging&left_version=1.0.0&right=prod&right_version=1.0.0", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("release-comparison %d: %s", recorder.Code, recorder.Body.String())
	}
	var resp struct {
		Changes []struct {
			ChangeID   string `json:"change_id"`
			Comparison string `json:"comparison"`
		} `json:"changes"`
		ChangeSummary struct {
			Added   int `json:"added"`
			Missing int `json:"missing"`
			Changed int `json:"changed"`
		} `json:"change_summary"`
		Consistent bool `json:"consistent"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode comparison: %v", err)
	}
	if resp.ChangeSummary.Added != 1 || resp.ChangeSummary.Missing != 1 || resp.ChangeSummary.Changed != 1 {
		t.Fatalf("change_summary = %+v", resp.ChangeSummary)
	}
	if resp.Consistent {
		t.Fatal("consistent must follow the per-entry conclusions and be false")
	}
	want := []string{"changed", "missing", "added"}
	if len(resp.Changes) != 3 {
		t.Fatalf("changes = %v", resp.Changes)
	}
	for i, change := range resp.Changes {
		if change.Comparison != want[i] {
			t.Fatalf("changes[%d] = %+v, want %s (dup pair then extra)", i, change, want[i])
		}
	}
}

func TestReleaseComparisonStaysConsistentWithRepeatedTitles(t *testing.T) {
	router, db := newDuplicateTitleRouter(t)
	for _, envName := range []string{"staging", "prod"} {
		registerScratchEnvironment(t, db, envName)
		insertScratchRecord(t, db, "", envName, "0.9.0", "allowed", "0.8.0",
			[]store.ChangeEntry{{Sequence: 1, Category: "base", Title: "base", Description: "b"}})
		insertScratchRecord(t, db, "", envName, "1.0.0", "allowed", "0.9.0", []store.ChangeEntry{
			dupEntry(1, "one"), dupEntry(2, "two"),
		})
	}
	recorder := doRequest(t, router, http.MethodGet,
		"/api/v1/release-comparison?left=staging&left_version=1.0.0&right=prod&right_version=1.0.0", "")
	var resp struct {
		Changes       []any `json:"changes"`
		ChangeSummary struct {
			Added   int `json:"added"`
			Missing int `json:"missing"`
			Changed int `json:"changed"`
		} `json:"change_summary"`
		Consistent bool `json:"consistent"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode comparison: %v", err)
	}
	if len(resp.Changes) != 0 {
		t.Fatalf("changes = %v, want empty array", resp.Changes)
	}
	if resp.ChangeSummary != struct {
		Added   int `json:"added"`
		Missing int `json:"missing"`
		Changed int `json:"changed"`
	}{} {
		t.Fatalf("change_summary = %+v, want zero counts", resp.ChangeSummary)
	}
	if !resp.Consistent {
		t.Fatal("identical repeated-title records must be consistent")
	}
}

func TestPromotionDiffsAndStateCountRepeatedTitles(t *testing.T) {
	router, db := newDuplicateTitleRouter(t)
	seedDuplicateComparison(t, router, db, "b-1")

	chain := doRequest(t, router, http.MethodGet,
		"/api/v1/release-batches/b-1/promotion-chain?environments=staging,prod", "")
	if chain.Code != http.StatusOK {
		t.Fatalf("promotion-chain %d: %s", chain.Code, chain.Body.String())
	}
	var chainResp struct {
		Segments []struct {
			Added        []any `json:"added_changes"`
			Missing      []any `json:"missing_changes"`
			Inconsistent []any `json:"inconsistent_changes"`
		} `json:"segment_diffs"`
		Consistent bool `json:"consistent"`
	}
	if err := json.Unmarshal(chain.Body.Bytes(), &chainResp); err != nil {
		t.Fatalf("decode chain: %v", err)
	}
	if chainResp.Consistent || len(chainResp.Segments) != 1 {
		t.Fatalf("chain = %+v", chainResp)
	}
	segment := chainResp.Segments[0]
	if len(segment.Added) != 1 || len(segment.Missing) != 1 || len(segment.Inconsistent) != 1 {
		t.Fatalf("segment = %+v", segment)
	}

	diff := doRequest(t, router, http.MethodGet,
		"/api/v1/release-batches/b-1/promotion-diff?from=staging&to=prod", "")
	if diff.Code != http.StatusOK {
		t.Fatalf("promotion-diff %d: %s", diff.Code, diff.Body.String())
	}

	trace := doRequest(t, router, http.MethodGet,
		"/api/v1/release-batches/b-1/changes/dup/trace?environments=staging,prod", "")
	if trace.Code != http.StatusOK {
		t.Fatalf("trace %d: %s", trace.Code, trace.Body.String())
	}

	// Rolling current staging 1.0.0 back to 0.9.0 loses both dup entries and
	// "solo" and restores "base": every entry participates in the totals.
	state := doRequest(t, router, http.MethodGet,
		"/api/v1/environments/staging/release-state?target_version=0.9.0", "")
	if state.Code != http.StatusOK {
		t.Fatalf("release-state %d: %s", state.Code, state.Body.String())
	}
	var stateResp struct {
		Impact *struct {
			Summary struct {
				Added   int `json:"added"`
				Missing int `json:"missing"`
				Changed int `json:"changed"`
			} `json:"change_summary"`
			Consistent bool `json:"consistent"`
		} `json:"rollback_impact"`
	}
	if err := json.Unmarshal(state.Body.Bytes(), &stateResp); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	if stateResp.Impact == nil {
		t.Fatal("rollback_impact missing")
	}
	if stateResp.Impact.Summary.Added != 1 || stateResp.Impact.Summary.Missing != 3 || stateResp.Impact.Summary.Changed != 0 {
		t.Fatalf("impact summary = %+v", stateResp.Impact.Summary)
	}
	if stateResp.Impact.Consistent {
		t.Fatal("rollback impact with entry differences must not be consistent")
	}
}
