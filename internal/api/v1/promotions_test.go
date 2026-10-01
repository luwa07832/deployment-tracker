package v1

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/luwa07832/deployment-tracker/internal/store"
)

func newChainRouter(t *testing.T) (*gin.Engine, *store.Store) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := store.Open(filepath.Join(t.TempDir(), "chain.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	engine := gin.New()
	Register(engine, Dependencies{Store: db})
	return engine, db
}

func insertFact(t *testing.T, db *store.Store, batch, env, version, gate, rollback, recordedAt string,
	entries ...store.ChangeEntry) *store.ReleaseRecord {
	t.Helper()
	if entries == nil {
		entries = []store.ChangeEntry{}
	}
	record := &store.ReleaseRecord{
		Environment:   env,
		Version:       version,
		GateStatus:    gate,
		RollbackPoint: rollback,
		BatchID:       batch,
		RecordedAt:    recordedAt,
		Changes:       entries,
	}
	if err := db.InsertReleaseRecord(record); err != nil {
		t.Fatalf("insert fact %s/%s: %v", env, version, err)
	}
	return record
}

func ce(seq int, category, title, description string) store.ChangeEntry {
	return store.ChangeEntry{Sequence: seq, Category: category, Title: title, Description: description}
}

func chainURL(batch, order string) string {
	return "/api/v1/release-batches/" + batch + "/chain?environments=" + order
}

func TestPromotionChainFullFlow(t *testing.T) {
	router, db := newChainRouter(t)
	for _, env := range []string{"dev", "test", "staging", "prod"} {
		registerEnvironmentOK(t, router, env)
	}
	insertFact(t, db, "b1", "dev", "1.0.0", "allowed", "snap:0.9", "2026-09-01T10:00:00Z",
		ce(1, "feature", "a", "a desc"))
	insertFact(t, db, "b1", "dev", "1.1.0", "allowed", "snap:1.0", "2026-09-02T10:00:00Z",
		ce(1, "feature", "a", "a desc"), ce(2, "fix", "b", "b desc"))
	insertFact(t, db, "b1", "test", "1.1.0", "allowed", "snap:1.0", "2026-09-03T10:00:00Z",
		ce(1, "feature", "a", "a desc"), ce(2, "fix", "b", "b desc"))
	insertFact(t, db, "b1", "staging", "1.1.0", "blocked", "snap:0.8", "2026-09-04T10:00:00Z",
		ce(1, "feature", "a", "a desc"))
	insertFact(t, db, "b1", "prod", "1.1.0", "allowed", "snap:1.0", "2026-09-05T10:00:00Z",
		ce(1, "feature", "a", "a desc"))

	recorder := doRequest(t, router, http.MethodGet, chainURL("b1", "dev,test,staging,prod"), "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	body := decodeBody(t, recorder)
	if body["batch_id"] != "b1" {
		t.Fatalf("batch_id = %v", body["batch_id"])
	}
	envs := body["environment_order"].([]any)
	if len(envs) != 4 || envs[0] != "dev" || envs[3] != "prod" {
		t.Fatalf("environment_order = %v", envs)
	}
	nodes := body["nodes"].([]any)
	devReleases := nodes[0].(map[string]any)["releases"].([]any)
	if len(devReleases) != 2 || devReleases[0].(map[string]any)["version"] != "1.0.0" {
		t.Fatalf("dev must list both releases chronologically: %v", devReleases)
	}
	segments := body["segment_diffs"].([]any)
	if len(segments) != 3 {
		t.Fatalf("segment_diffs = %v", segments)
	}
	if body["consistent"] != false {
		t.Fatalf("consistent = %v, want false", body["consistent"])
	}
	joined := ""
	for _, category := range body["inconsistency_categories"].([]any) {
		joined += category.(string) + ","
	}
	for _, want := range []string{"version_divergence", "gate_status_conflict", "rollback_point_conflict", "missing_changes"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("categories %v missing %s", body["inconsistency_categories"], want)
		}
	}
	// dev -> test carries an extra 1.0.0 on dev: version divergence is reported.
	first := segments[0].(map[string]any)
	divergences := first["version_divergences"].([]any)
	if len(divergences) != 1 || divergences[0].(map[string]any)["version"] != "1.0.0" {
		t.Fatalf("dev->test divergences = %v", divergences)
	}
	if first["consistent"] != false {
		t.Fatalf("dev->test consistent should be false: %v", first)
	}
	// test -> staging: b is missing on staging.
	second := segments[1].(map[string]any)
	missing := second["missing_changes"].([]any)
	if len(missing) != 1 || missing[0].(map[string]any)["title"] != "b" {
		t.Fatalf("missing b on staging: %v", missing)
	}
}

func TestPromotionChainConsistentWhenIdentical(t *testing.T) {
	router, db := newChainRouter(t)
	registerEnvironmentOK(t, router, "dev")
	registerEnvironmentOK(t, router, "test")
	entries := []store.ChangeEntry{ce(1, "feature", "a", "a desc")}
	insertFact(t, db, "b2", "dev", "1.0.0", "allowed", "snap:1", "2026-09-01T10:00:00Z", entries...)
	insertFact(t, db, "b2", "test", "1.0.0", "allowed", "snap:1", "2026-09-02T10:00:00Z", entries...)

	recorder := doRequest(t, router, http.MethodGet, chainURL("b2", "dev,test"), "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	body := decodeBody(t, recorder)
	if body["consistent"] != true {
		t.Fatalf("consistent = %v want true", body["consistent"])
	}
	segment := body["segment_diffs"].([]any)[0].(map[string]any)
	for key, want := range map[string]int{
		"version_divergences": 0, "gate_status_conflicts": 0,
		"rollback_point_conflicts": 0, "added_changes": 0,
		"missing_changes": 0, "inconsistent_changes": 0,
	} {
		if len(segment[key].([]any)) != want {
			t.Fatalf("%s = %v, want empty", key, segment[key])
		}
	}
	if segment["consistent"] != true {
		t.Fatalf("segment consistent = %v", segment["consistent"])
	}
}

func TestPromotionChainAddedChangesStayConsistent(t *testing.T) {
	router, db := newChainRouter(t)
	registerEnvironmentOK(t, router, "dev")
	registerEnvironmentOK(t, router, "test")
	insertFact(t, db, "b3", "dev", "1.0.0", "allowed", "snap:1", "2026-09-01T10:00:00Z",
		ce(1, "feature", "a", "a desc"))
	insertFact(t, db, "b3", "test", "1.0.0", "allowed", "snap:1", "2026-09-02T10:00:00Z",
		ce(1, "feature", "a", "a desc"), ce(2, "feature", "c", "c desc"))

	recorder := doRequest(t, router, http.MethodGet, chainURL("b3", "dev,test"), "")
	body := decodeBody(t, recorder)
	segment := body["segment_diffs"].([]any)[0].(map[string]any)
	added := segment["added_changes"].([]any)
	if len(added) != 1 || added[0].(map[string]any)["title"] != "c" {
		t.Fatalf("added = %v", added)
	}
	if body["consistent"] != true {
		t.Fatalf("added-only segment must stay consistent: %v", body)
	}
}

func TestPromotionChainInconsistentChange(t *testing.T) {
	router, db := newChainRouter(t)
	registerEnvironmentOK(t, router, "dev")
	registerEnvironmentOK(t, router, "test")
	insertFact(t, db, "b4", "dev", "1.0.0", "allowed", "snap:1", "2026-09-01T10:00:00Z",
		ce(1, "feature", "a", "old"))
	insertFact(t, db, "b4", "test", "1.0.0", "allowed", "snap:1", "2026-09-02T10:00:00Z",
		ce(1, "fix", "a", "new"))

	recorder := doRequest(t, router, http.MethodGet, chainURL("b4", "dev,test"), "")
	body := decodeBody(t, recorder)
	if body["consistent"] != false {
		t.Fatalf("consistent = %v want false", body["consistent"])
	}
	segment := body["segment_diffs"].([]any)[0].(map[string]any)
	changed := segment["inconsistent_changes"].([]any)
	if len(changed) != 1 {
		t.Fatalf("inconsistent_changes = %v", changed)
	}
	pair := changed[0].(map[string]any)
	if pair["left"].(map[string]any)["category"] != "feature" ||
		pair["right"].(map[string]any)["category"] != "fix" {
		t.Fatalf("changed pair = %v", pair)
	}
}

func TestBatchIsolation(t *testing.T) {
	router, db := newChainRouter(t)
	registerEnvironmentOK(t, router, "dev")
	registerEnvironmentOK(t, router, "test")
	// Same environment + version reused by two distinct batches must both persist.
	insertFact(t, db, "batch-x", "dev", "2.0.0", "allowed", "snap:x", "2026-09-01T10:00:00Z",
		ce(1, "feature", "x", "x desc"))
	insertFact(t, db, "batch-y", "dev", "2.0.0", "allowed", "snap:y", "2026-09-01T11:00:00Z",
		ce(1, "feature", "y", "y desc"))
	insertFact(t, db, "batch-x", "test", "2.0.0", "allowed", "snap:x", "2026-09-02T10:00:00Z",
		ce(1, "feature", "x", "x desc"))
	insertFact(t, db, "batch-y", "test", "2.0.0", "allowed", "snap:y", "2026-09-02T11:00:00Z",
		ce(1, "feature", "y", "y desc"))

	for batch, wantTitle := range map[string]string{"batch-x": "x", "batch-y": "y"} {
		recorder := doRequest(t, router, http.MethodGet, chainURL(batch, "dev,test"), "")
		if recorder.Code != http.StatusOK {
			t.Fatalf("batch %s status %d: %s", batch, recorder.Code, recorder.Body.String())
		}
		body := decodeBody(t, recorder)
		if body["batch_id"] != batch {
			t.Fatalf("batch_id = %v", body["batch_id"])
		}
		nodeReleases := body["nodes"].([]any)[0].(map[string]any)["releases"].([]any)
		if len(nodeReleases) != 1 {
			t.Fatalf("batch %s must not merge facts: %v", batch, nodeReleases)
		}
		changes := nodeReleases[0].(map[string]any)["changes"].([]any)
		if changes[0].(map[string]any)["title"] != wantTitle {
			t.Fatalf("batch %s title = %v want %s", batch, changes, wantTitle)
		}
	}
}

func TestBatchNotFoundIsUniqueObservable(t *testing.T) {
	router, _ := newChainRouter(t)
	registerEnvironmentOK(t, router, "dev")
	recorder := doRequest(t, router, http.MethodGet, chainURL("missing", "dev"), "")
	wantError(t, recorder, http.StatusNotFound, store.CodeReleaseBatchNotFoundV1)
}

func TestDuplicateAndUnknownEnvironments(t *testing.T) {
	router, db := newChainRouter(t)
	registerEnvironmentOK(t, router, "dev")
	insertFact(t, db, "b5", "dev", "1.0.0", "allowed", "snap:1", "2026-09-01T10:00:00Z")

	dup := doRequest(t, router, http.MethodGet, chainURL("b5", "dev,test,dev"), "")
	wantError(t, dup, http.StatusBadRequest, store.CodeDuplicateEnvironmentChainV1)
	if !strings.Contains(dup.Body.String(), "dev") {
		t.Fatalf("duplicate message must name dev: %s", dup.Body.String())
	}

	unknown := doRequest(t, router, http.MethodGet, chainURL("b5", "dev,ghost"), "")
	wantError(t, unknown, http.StatusBadRequest, store.CodeUnknownEnvironmentChainV1)
	if !strings.Contains(unknown.Body.String(), "ghost") {
		t.Fatalf("unknown message must name ghost: %s", unknown.Body.String())
	}
}

func TestOrderConflictWhenFactsCannotOrder(t *testing.T) {
	router, db := newChainRouter(t)
	registerEnvironmentOK(t, router, "dev")
	registerEnvironmentOK(t, router, "test")
	insertFact(t, db, "b6", "dev", "1.0.0", "allowed", "snap:1", "2026-09-03T10:00:00Z",
		ce(1, "feature", "a", "a"))
	// prod fact is earlier than dev, contradicting dev -> prod promotion.
	insertFact(t, db, "b6", "test", "1.0.0", "allowed", "snap:1", "2026-09-01T10:00:00Z",
		ce(1, "feature", "a", "a"))
	recorder := doRequest(t, router, http.MethodGet, chainURL("b6", "dev,test"), "")
	wantError(t, recorder, http.StatusConflict, store.CodePromotionChainConflictV1)
	if !strings.Contains(recorder.Body.String(), "dev") || !strings.Contains(recorder.Body.String(), "test") {
		t.Fatalf("conflict must name nodes: %s", recorder.Body.String())
	}
}

func TestOrderConflictWhenNodeHasNoFacts(t *testing.T) {
	router, db := newChainRouter(t)
	registerEnvironmentOK(t, router, "dev")
	registerEnvironmentOK(t, router, "prod")
	insertFact(t, db, "b7", "dev", "1.0.0", "allowed", "snap:1", "2026-09-01T10:00:00Z")
	recorder := doRequest(t, router, http.MethodGet, chainURL("b7", "dev,prod"), "")
	wantError(t, recorder, http.StatusConflict, store.CodePromotionChainConflictV1)
	if !strings.Contains(recorder.Body.String(), "prod") {
		t.Fatalf("conflict must point at prod: %s", recorder.Body.String())
	}
}

func TestLocalPromotionDiff(t *testing.T) {
	router, db := newChainRouter(t)
	for _, env := range []string{"dev", "test", "staging", "prod"} {
		registerEnvironmentOK(t, router, env)
	}
	insertFact(t, db, "b8", "staging", "1.0.0", "allowed", "snap:1", "2026-09-03T10:00:00Z",
		ce(1, "feature", "a", "a desc"))
	insertFact(t, db, "b8", "prod", "1.0.0", "blocked", "snap:0", "2026-09-04T10:00:00Z",
		ce(1, "feature", "a", "a desc"))

	url := "/api/v1/release-batches/b8/promotion-diff?from=staging&to=prod"
	recorder := doRequest(t, router, http.MethodGet, url, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d: %s", recorder.Code, recorder.Body.String())
	}
	body := decodeBody(t, recorder)
	if body["from_environment"] != "staging" || body["to_environment"] != "prod" {
		t.Fatalf("sides = %v/%v", body["from_environment"], body["to_environment"])
	}
	segment := body["segment"].(map[string]any)
	if segment["consistent"] != false {
		t.Fatalf("segment consistent = %v", segment["consistent"])
	}
	gates := segment["gate_status_conflicts"].([]any)
	if len(gates) != 1 || gates[0].(map[string]any)["version"] != "1.0.0" {
		t.Fatalf("gate conflicts = %v", gates)
	}
}

func TestLocalPromotionDiffValidation(t *testing.T) {
	router, db := newChainRouter(t)
	registerEnvironmentOK(t, router, "dev")
	registerEnvironmentOK(t, router, "test")
	insertFact(t, db, "b9", "dev", "1.0.0", "allowed", "snap:1", "2026-09-01T10:00:00Z")
	insertFact(t, db, "b9", "test", "1.0.0", "allowed", "snap:1", "2026-09-02T10:00:00Z")

	missing := doRequest(t, router, http.MethodGet,
		"/api/v1/release-batches/b9/promotion-diff?from=dev", "")
	wantError(t, missing, http.StatusBadRequest, store.CodeInvalidRequest)

	same := doRequest(t, router, http.MethodGet,
		"/api/v1/release-batches/b9/promotion-diff?from=dev&to=dev", "")
	wantError(t, same, http.StatusBadRequest, store.CodeSameEnvironmentCompareV1)

	unknown := doRequest(t, router, http.MethodGet,
		"/api/v1/release-batches/b9/promotion-diff?from=dev&to=ghost", "")
	wantError(t, unknown, http.StatusBadRequest, store.CodeUnknownEnvironmentChainV1)

	noBatch := doRequest(t, router, http.MethodGet,
		"/api/v1/release-batches/nope/promotion-diff?from=dev&to=test", "")
	wantError(t, noBatch, http.StatusNotFound, store.CodeReleaseBatchNotFoundV1)
}

func TestChangeTrace(t *testing.T) {
	router, db := newChainRouter(t)
	for _, env := range []string{"dev", "test", "staging", "prod"} {
		registerEnvironmentOK(t, router, env)
	}
	insertFact(t, db, "b10", "dev", "1.0.0", "allowed", "snap:1", "2026-09-01T10:00:00Z",
		ce(1, "feature", "a", "a"), ce(2, "fix", "b", "b"))
	insertFact(t, db, "b10", "test", "1.0.0", "allowed", "snap:1", "2026-09-02T10:00:00Z",
		ce(1, "feature", "a", "a"), ce(2, "fix", "b", "b"))
	// staging drops b.
	insertFact(t, db, "b10", "staging", "1.0.0", "allowed", "snap:1", "2026-09-03T10:00:00Z",
		ce(1, "feature", "a", "a"))
	insertFact(t, db, "b10", "prod", "1.0.0", "allowed", "snap:1", "2026-09-04T10:00:00Z",
		ce(1, "feature", "a", "a"))

	url := "/api/v1/release-batches/b10/changes/b/trace?environments=dev,test,staging,prod"
	recorder := doRequest(t, router, http.MethodGet, url, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d: %s", recorder.Code, recorder.Body.String())
	}
	body := decodeBody(t, recorder)
	if body["first_present_environment"] != "dev" {
		t.Fatalf("first present = %v", body["first_present_environment"])
	}
	if body["first_missing_environment"] != "staging" {
		t.Fatalf("first missing = %v", body["first_missing_environment"])
	}
	present := body["present_environments"].([]any)
	if len(present) != 2 || present[0] != "dev" || present[1] != "test" {
		t.Fatalf("present environments = %v", present)
	}
	appearances := body["appearances"].([]any)
	if len(appearances) != 2 {
		t.Fatalf("appearances = %v", appearances)
	}
}

func TestChangeTraceUnknownChange(t *testing.T) {
	router, db := newChainRouter(t)
	registerEnvironmentOK(t, router, "dev")
	registerEnvironmentOK(t, router, "test")
	insertFact(t, db, "b11", "dev", "1.0.0", "allowed", "snap:1", "2026-09-01T10:00:00Z",
		ce(1, "feature", "a", "a"))
	insertFact(t, db, "b11", "test", "1.0.0", "allowed", "snap:1", "2026-09-02T10:00:00Z",
		ce(1, "feature", "a", "a"))

	url := "/api/v1/release-batches/b11/changes/zzz/trace?environments=dev,test"
	recorder := doRequest(t, router, http.MethodGet, url, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d: %s", recorder.Code, recorder.Body.String())
	}
	body := decodeBody(t, recorder)
	if body["first_present_environment"] != nil || body["first_missing_environment"] != nil {
		t.Fatalf("unknown change must yield null anchors: %v", body)
	}
	if len(body["present_environments"].([]any)) != 0 || len(body["appearances"].([]any)) != 0 {
		t.Fatalf("unknown change must yield empty lists: %v", body)
	}
}

func TestChangeTraceBatchAndSequenceErrors(t *testing.T) {
	router, _ := newChainRouter(t)
	registerEnvironmentOK(t, router, "dev")
	noBatch := doRequest(t, router, http.MethodGet,
		"/api/v1/release-batches/nope/changes/a/trace?environments=dev", "")
	wantError(t, noBatch, http.StatusNotFound, store.CodeReleaseBatchNotFoundV1)

	dup := doRequest(t, router, http.MethodGet,
		"/api/v1/release-batches/nope/changes/a/trace?environments=dev,dev", "")
	wantError(t, dup, http.StatusBadRequest, store.CodeDuplicateEnvironmentChainV1)
}

func TestChainQueryIsDeterministic(t *testing.T) {
	router, db := newChainRouter(t)
	for _, env := range []string{"dev", "test"} {
		registerEnvironmentOK(t, router, env)
	}
	insertFact(t, db, "b12", "dev", "1.0.0", "allowed", "snap:1", "2026-09-01T10:00:00Z",
		ce(2, "fix", "b", "b"), ce(1, "feature", "a", "a"))
	insertFact(t, db, "b12", "test", "1.0.0", "allowed", "snap:1", "2026-09-02T10:00:00Z",
		ce(2, "fix", "b", "b"), ce(1, "feature", "a", "a"))

	url := chainURL("b12", "dev,test")
	first := doRequest(t, router, http.MethodGet, url, "")
	second := doRequest(t, router, http.MethodGet, url, "")
	if first.Body.String() != second.Body.String() {
		t.Fatalf("repeated queries differ:\n%s\n%s", first.Body.String(), second.Body.String())
	}
}

func TestRepeatedReleaseInSameEnvironmentListedChronologically(t *testing.T) {
	router, db := newChainRouter(t)
	registerEnvironmentOK(t, router, "dev")
	registerEnvironmentOK(t, router, "test")
	insertFact(t, db, "b13", "dev", "1.0.0", "allowed", "snap:1", "2026-09-01T10:00:00Z",
		ce(1, "feature", "a", "a"))
	insertFact(t, db, "b13", "dev", "1.1.0", "allowed", "snap:1", "2026-09-02T10:00:00Z",
		ce(1, "feature", "a", "a"))
	insertFact(t, db, "b13", "test", "1.1.0", "allowed", "snap:1", "2026-09-03T10:00:00Z",
		ce(1, "feature", "a", "a"))

	recorder := doRequest(t, router, http.MethodGet, chainURL("b13", "dev,test"), "")
	body := decodeBody(t, recorder)
	releases := body["nodes"].([]any)[0].(map[string]any)["releases"].([]any)
	if len(releases) != 2 {
		t.Fatalf("both dev releases must be listed: %v", releases)
	}
	if releases[0].(map[string]any)["recorded_at"] != "2026-09-01T10:00:00Z" ||
		releases[1].(map[string]any)["recorded_at"] != "2026-09-02T10:00:00Z" {
		t.Fatalf("releases not chronological: %v", releases)
	}
}
