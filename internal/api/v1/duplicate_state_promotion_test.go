package v1

import (
	"net/http"
	"testing"

	"github.com/luwa07832/deployment-tracker/internal/store"
)

// release-state rollback_impact uses the same per-entry pairing: every
// repeated title participates, and consistent follows the totals.
func TestReleaseStateRepeatedTitleRollbackImpact(t *testing.T) {
	router, deps := newTestRouterWithDeps(t)
	legacyEnv(t, deps, "prod")
	// Target version 0.9.0 has two "x" entries; current 1.0.0 has one "x" and
	// one "new". Direction is current -> target.
	legacyDuplicateRecord(t, deps, "prod", "0.9.0", "allowed", "0.9.0",
		dupEntry(1, "feature", "x", "one"),
		dupEntry(2, "feature", "x", "two"))
	legacyDuplicateRecord(t, deps, "prod", "1.0.0", "allowed", "0.9.0",
		dupEntry(1, "feature", "x", "one"),
		dupEntry(3, "feature", "new", "d"))

	recorder := doRequest(t, router, http.MethodGet,
		"/api/v1/environments/prod/release-state?target_version=0.9.0", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	impact := decodeBody(t, recorder)["rollback_impact"].(map[string]any)
	summary := impact["change_summary"].(map[string]any)
	// current -> target: target's second x is "added", current's "new" is
	// "missing".
	if summary["added"] != float64(1) || summary["missing"] != float64(1) || summary["changed"] != float64(0) {
		t.Fatalf("change_summary = %v, want added=1 missing=1", summary)
	}
	if impact["consistent"] != false {
		t.Fatalf("consistent = %v, want false", impact["consistent"])
	}
	changes := impact["changes"].([]any)
	if len(changes) != 2 {
		t.Fatalf("changes = %v, want new and second x", changes)
	}
	first := changes[0].(map[string]any)
	second := changes[1].(map[string]any)
	if first["change_id"] != "new" || first["comparison"] != "missing" {
		t.Fatalf("first change = %v, want missing new", first)
	}
	if second["change_id"] != "x" || second["comparison"] != "added" {
		t.Fatalf("second change = %v, want added x", second)
	}
}

// A current release identical to its rollback target, repeated titles
// included, yields an empty consistent impact.
func TestReleaseStateRepeatedTitleSelfConsistent(t *testing.T) {
	router, deps := newTestRouterWithDeps(t)
	legacyEnv(t, deps, "prod")
	changes := []store.ChangeEntry{
		dupEntry(1, "feature", "x", "one"),
		dupEntry(2, "feature", "x", "two"),
	}
	legacyDuplicateRecord(t, deps, "prod", "0.9.0", "allowed", "0.9.0",
		changes...)
	// The current release is also the requested target, carrying the same
	// repeated set; the impact must be empty and consistent.
	legacyDuplicateRecord(t, deps, "prod", "1.0.0", "allowed", "0.9.0",
		changes...)

	recorder := doRequest(t, router, http.MethodGet,
		"/api/v1/environments/prod/release-state?target_version=1.0.0", "")
	impact := decodeBody(t, recorder)["rollback_impact"].(map[string]any)
	if impact["consistent"] != true {
		t.Fatalf("consistent = %v, want true", impact["consistent"])
	}
	if changes := impact["changes"].([]any); len(changes) != 0 {
		t.Fatalf("changes = %v, want empty", changes)
	}
}

// promotion-chain segment diffs keep every repeated entry and omit segments
// promotion-chain segment diffs keep every repeated entry and omit segments
// whose repeated sets match fully.
func TestPromotionChainRepeatedTitles(t *testing.T) {
	router, deps := newTestRouterWithDeps(t)
	for _, env := range []string{"dev", "staging", "prod"} {
		legacyEnv(t, deps, env)
	}
	batch := "b-dup"
	insert := func(env string, changes ...store.ChangeEntry) {
		t.Helper()
		record := &store.ReleaseRecord{
			Environment:   env,
			Version:       "1.0.0",
			BatchID:       batch,
			GateStatus:    "allowed",
			RollbackPoint: "rb:1",
			Changes:       changes,
		}
		if err := deps.Store.InsertReleaseRecord(record); err != nil {
			t.Fatalf("insert %s: %v", env, err)
		}
	}
	// dev -> staging identical repeated sets: no segment.
	insert("dev",
		dupEntry(1, "feature", "x", "one"),
		dupEntry(2, "feature", "x", "two"))
	insert("staging",
		dupEntry(1, "feature", "x", "one"),
		dupEntry(2, "feature", "x", "two"))
	// staging -> prod: prod dropped the second x and changed nothing else.
	insert("prod",
		dupEntry(1, "feature", "x", "one"))

	recorder := doRequest(t, router, http.MethodGet,
		"/api/v1/release-batches/"+batch+"/promotion-chain?environments=dev,staging,prod", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	body := decodeBody(t, recorder)
	if body["consistent"] != false {
		t.Fatalf("consistent = %v, want false", body["consistent"])
	}
	segments := body["segment_diffs"].([]any)
	if len(segments) != 1 {
		t.Fatalf("segments = %v, want only staging->prod", segments)
	}
	segment := segments[0].(map[string]any)
	if segment["from_environment"] != "staging" || segment["to_environment"] != "prod" {
		t.Fatalf("segment endpoints = %v", segment)
	}
	missing := segment["missing_changes"].([]any)
	if len(missing) != 1 || missing[0].(map[string]any)["title"] != "x" ||
		missing[0].(map[string]any)["sequence"] != float64(2) {
		t.Fatalf("missing = %v, want the second x", missing)
	}
}

// promotion-diff reports every repeated entry on both sides.
func TestPromotionDiffRepeatedTitles(t *testing.T) {
	router, deps := newTestRouterWithDeps(t)
	legacyEnv(t, deps, "dev")
	legacyEnv(t, deps, "prod")
	batch := "b-dup2"
	insert := func(env string, changes ...store.ChangeEntry) {
		t.Helper()
		record := &store.ReleaseRecord{
			Environment:   env,
			Version:       "1.0.0",
			BatchID:       batch,
			GateStatus:    "allowed",
			RollbackPoint: "rb:1",
			Changes:       changes,
		}
		if err := deps.Store.InsertReleaseRecord(record); err != nil {
			t.Fatalf("insert %s: %v", env, err)
		}
	}
	insert("dev",
		dupEntry(1, "feature", "x", "one"),
		dupEntry(2, "fix", "x", "changed on prod"))
	insert("prod",
		dupEntry(1, "feature", "x", "one"),
		dupEntry(2, "feature", "x", "changed on prod"),
		dupEntry(3, "feature", "extra", "d"))

	recorder := doRequest(t, router, http.MethodGet,
		"/api/v1/release-batches/"+batch+"/promotion-diff?from=dev&to=prod", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	diff := decodeBody(t, recorder)["diff"].(map[string]any)
	added := diff["added_changes"].([]any)
	inconsistent := diff["inconsistent_changes"].([]any)
	missing := diff["missing_changes"].([]any)
	if len(added) != 1 || added[0].(map[string]any)["title"] != "extra" {
		t.Fatalf("added = %v, want extra", added)
	}
	if len(missing) != 0 {
		t.Fatalf("missing = %v, want none", missing)
	}
	if len(inconsistent) != 1 ||
		inconsistent[0].(map[string]any)["left"].(map[string]any)["category"] != "fix" ||
		inconsistent[0].(map[string]any)["right"].(map[string]any)["category"] != "feature" {
		t.Fatalf("inconsistent = %v, want second x category mismatch", inconsistent)
	}
}

// Legacy repeated titles are returned in full and in stored/sequence order
// by the read endpoints; history is never rewritten.
func TestReadEndpointsReturnAllRepeatedEntriesInOrder(t *testing.T) {
	router, deps := newTestRouterWithDeps(t)
	legacyEnv(t, deps, "prod")
	record := legacyDuplicateRecord(t, deps, "prod", "1.0.0", "allowed", "0.9.0",
		dupEntry(1, "feature", "dup", "first"),
		dupEntry(2, "fix", "dup", "second"))

	recorder := doRequest(t, router, http.MethodGet,
		"/api/v1/release-records/"+record.PublicID, "")
	changes := decodeBody(t, recorder)["release_record"].(map[string]any)["changes"].([]any)
	if len(changes) != 2 {
		t.Fatalf("changes = %v, want both repeated entries", changes)
	}
	if changes[0].(map[string]any)["description"] != "first" ||
		changes[1].(map[string]any)["description"] != "second" {
		t.Fatalf("order/content changed: %v", changes)
	}
}
