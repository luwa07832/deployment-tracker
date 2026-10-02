package store

import (
	"errors"
	"path/filepath"
	"testing"
)

func newGateTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "gate.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.EnsureEnvironment(&TrackedEnvironment{Environment: "prod"}); err != nil {
		t.Fatalf("ensure environment: %v", err)
	}
	return db
}

func gateTestRecord(environment, version, gateStatus string) *ReleaseRecord {
	return &ReleaseRecord{
		Environment:   environment,
		Version:       version,
		GateStatus:    gateStatus,
		RollbackPoint: "snapshot:base",
		Changes:       []ChangeEntry{},
	}
}

func gateCheck(name, status, evidence string) GateCheck {
	check := GateCheck{Name: name, Status: status, Evidence: evidence}
	if status == GateCheckWaived {
		check.WaiverReason = "accepted risk for " + name
	}
	return check
}

func TestDeriveGateStatusPriorities(t *testing.T) {
	cases := []struct {
		name   string
		checks []GateCheck
		want   string
	}{
		{"all passed", []GateCheck{
			gateCheck("a", GateCheckPassed, "e"), gateCheck("b", GateCheckWaived, "e"),
		}, "allowed"},
		{"pending present", []GateCheck{
			gateCheck("a", GateCheckPassed, "e"), gateCheck("b", GateCheckPending, "e"),
		}, "pending"},
		{"failed wins over pending", []GateCheck{
			gateCheck("a", GateCheckFailed, "e"), gateCheck("b", GateCheckPending, "e"),
		}, "blocked"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DeriveGateStatus(tc.checks); got != tc.want {
				t.Fatalf("derive = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSaveGateEvaluationStoresSnapshotAndIsIdempotent(t *testing.T) {
	db := newGateTestStore(t)
	record := gateTestRecord("prod", "1.2.0", "blocked")
	if err := db.InsertReleaseRecord(record); err != nil {
		t.Fatalf("insert record: %v", err)
	}

	checks := []GateCheck{
		{Position: 1, Name: "deploy-test", Status: GateCheckFailed, Evidence: "log:1"},
		{Position: 2, Name: "canary", Status: GateCheckPassed, Evidence: "log:2"},
	}
	saved, existing, err := db.SaveGateEvaluation(record.PublicID, checks)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if existing {
		t.Fatalf("first save must not report existing")
	}
	if saved.GateStatus != "blocked" || saved.EffectiveGateStatus != "blocked" {
		t.Fatalf("gate statuses = %q/%q, want blocked/blocked", saved.GateStatus, saved.EffectiveGateStatus)
	}
	if len(saved.Checks) != 2 || saved.Checks[0].Name != "canary" || saved.Checks[1].Name != "deploy-test" {
		t.Fatalf("checks not sorted by check_name: %+v", saved.Checks)
	}

	// Reorder submission but keep identical content: the same snapshot returns.
	reordered := []GateCheck{checks[1], checks[0]}
	again, existing, err := db.SaveGateEvaluation(record.PublicID, reordered)
	if err != nil {
		t.Fatalf("save again: %v", err)
	}
	if !existing {
		t.Fatalf("identical resubmission must report existing")
	}
	if again.ID != saved.ID {
		t.Fatalf("identical resubmission created a new evaluation %d != %d", again.ID, saved.ID)
	}

	// Different content that still derives blocked: different evidence plus an
	// extra check. A set deriving another status answers GATE_STATUS_MISMATCH
	// before the snapshot content conflict is even considered.
	different := []GateCheck{
		{Position: 1, Name: "deploy-test", Status: GateCheckFailed, Evidence: "log:3"},
		{Position: 2, Name: "canary", Status: GateCheckPassed, Evidence: "log:2"},
		{Position: 3, Name: "manual-signoff", Status: GateCheckWaived, Evidence: "ticket:9", WaiverReason: "accepted"},
	}
	_, _, err = db.SaveGateEvaluation(record.PublicID, different)
	var conflict *ErrGateEvaluationConflict
	if !errors.As(err, &conflict) {
		t.Fatalf("different resubmission = %v, want ErrGateEvaluationConflict", err)
	}

	read, err := db.GetGateEvaluation(record.PublicID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if read.GateStatus != "blocked" {
		t.Fatalf("conflict must not rewrite the snapshot, got %q", read.GateStatus)
	}
}

func TestSaveGateEvaluationStatusMismatchAndUnknownRecord(t *testing.T) {
	db := newGateTestStore(t)
	record := gateTestRecord("prod", "1.2.0", "allowed")
	if err := db.InsertReleaseRecord(record); err != nil {
		t.Fatalf("insert record: %v", err)
	}
	checks := []GateCheck{gateCheck("pending-check", GateCheckPending, "log")}
	_, _, err := db.SaveGateEvaluation(record.PublicID, checks)
	var mismatch *ErrGateStatusMismatch
	if !errors.As(err, &mismatch) || mismatch.RecordStatus != "allowed" || mismatch.CheckStatus != "pending" {
		t.Fatalf("mismatch err = %v", err)
	}
	if read, err := db.GetGateEvaluation(record.PublicID); err != nil || read != nil {
		t.Fatalf("mismatch must not store a snapshot, got %+v err %v", read, err)
	}

	_, _, err = db.SaveGateEvaluation("rel_does_not_exist", checks)
	if !errors.Is(err, ErrGateEvaluationRecordNotFound) {
		t.Fatalf("unknown record = %v, want ErrGateEvaluationRecordNotFound", err)
	}
}

func TestListGateEvaluationsPageFiltersAndPaginates(t *testing.T) {
	db := newGateTestStore(t)
	specs := []struct {
		version    string
		gateStatus string
		checks     []GateCheck
	}{
		{"1.0.0", "allowed", []GateCheck{gateCheck("only-passed", GateCheckPassed, "e")}},
		{"1.1.0", "pending", []GateCheck{gateCheck("pending-check", GateCheckPending, "e")}},
		{"1.2.0", "blocked", []GateCheck{gateCheck("failed-check", GateCheckFailed, "e")}},
	}
	records := make([]*ReleaseRecord, len(specs))
	for i, spec := range specs {
		record := gateTestRecord("prod", spec.version, spec.gateStatus)
		if err := db.InsertReleaseRecord(record); err != nil {
			t.Fatalf("insert %s: %v", spec.version, err)
		}
		if _, _, err := db.SaveGateEvaluation(record.PublicID, spec.checks); err != nil {
			t.Fatalf("save %s: %v", spec.version, err)
		}
		records[i] = record
	}

	all, err := db.ListGateEvaluationsPage(GateEvaluationFilter{}, 0, 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("list length = %d, want 3", len(all))
	}
	if all[0].Version != "1.2.0" || all[2].Version != "1.0.0" {
		t.Fatalf("list not newest first: %s then %s", all[0].Version, all[2].Version)
	}

	blocked, err := db.ListGateEvaluationsPage(GateEvaluationFilter{GateStatus: "blocked"}, 0, 100)
	if err != nil {
		t.Fatalf("filter gate status: %v", err)
	}
	if len(blocked) != 1 || blocked[0].Version != "1.2.0" {
		t.Fatalf("blocked filter = %+v", blocked)
	}

	withPending, err := db.ListGateEvaluationsPage(GateEvaluationFilter{CheckStatus: GateCheckPending}, 0, 100)
	if err != nil {
		t.Fatalf("filter check status: %v", err)
	}
	if len(withPending) != 1 || withPending[0].Version != "1.1.0" {
		t.Fatalf("check_status filter = %+v", withPending)
	}

	page1, err := db.ListGateEvaluationsPage(GateEvaluationFilter{}, 0, 2)
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if len(page1) != 3 {
		t.Fatalf("page 1 should fetch limit+1 rows, got %d", len(page1))
	}
	page2, err := db.ListGateEvaluationsPage(GateEvaluationFilter{}, page1[1].ID, 2)
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if len(page2) != 1 || page2[0].Version != "1.0.0" {
		t.Fatalf("page 2 = %+v", page2)
	}
}

func TestResolveGateEvaluationPosition(t *testing.T) {
	db := newGateTestStore(t)
	record := gateTestRecord("prod", "1.2.0", "allowed")
	if err := db.InsertReleaseRecord(record); err != nil {
		t.Fatalf("insert record: %v", err)
	}
	if id, ok, err := db.ResolveGateEvaluationPosition(record.PublicID); err != nil || ok {
		t.Fatalf("before save position = %d %t %v", id, ok, err)
	}
	saved, _, err := db.SaveGateEvaluation(record.PublicID, []GateCheck{gateCheck("a", GateCheckPassed, "e")})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	id, ok, err := db.ResolveGateEvaluationPosition(record.PublicID)
	if err != nil || !ok || id != saved.ID {
		t.Fatalf("position = %d %t %v, want %d", id, ok, err, saved.ID)
	}
}
