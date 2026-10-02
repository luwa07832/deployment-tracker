package store

import (
	"errors"
	"testing"
)

func gateChecks(namesAndStatus ...string) []GateCheck {
	checks := make([]GateCheck, 0, len(namesAndStatus)/2)
	for i := 0; i+1 < len(namesAndStatus); i += 2 {
		check := GateCheck{
			Name:     namesAndStatus[i],
			Status:   namesAndStatus[i+1],
			Evidence: "evidence for " + namesAndStatus[i],
		}
		if check.Status == GateCheckWaived {
			check.WaiverReason = "accepted risk for " + namesAndStatus[i]
		}
		checks = append(checks, check)
	}
	return checks
}

func seedRecordForGate(t *testing.T, db *Store, environment, version, gate string) *ReleaseRecord {
	t.Helper()
	if _, err := db.EnsureEnvironment(&TrackedEnvironment{Environment: environment}); err != nil {
		t.Fatalf("ensure env: %v", err)
	}
	record := sampleRecord(environment, version, gate, "rb:"+version, entries("a")...)
	if err := db.InsertReleaseRecord(record); err != nil {
		t.Fatalf("insert record: %v", err)
	}
	return record
}

func TestSaveGateEvaluationStoresSnapshotAndChecksSorted(t *testing.T) {
	db := openTestStore(t)
	record := seedRecordForGate(t, db, "prod", "1.0.0", "blocked")

	// Submit out of check_name order; the stored view must sort by name.
	submitted := gateChecks("zeta", "passed", "alpha", "failed", "mid", "waived")
	evaluation, created, err := db.SaveGateEvaluation(record.PublicID, submitted)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if !created {
		t.Fatal("first save must report created")
	}
	if evaluation.GateStatus != "blocked" || evaluation.EffectiveGateStatus != "blocked" {
		t.Fatalf("statuses = %q/%q, want blocked/blocked", evaluation.GateStatus, evaluation.EffectiveGateStatus)
	}
	if evaluation.PublicID != record.PublicID || evaluation.Environment != "prod" ||
		evaluation.Version != "1.0.0" {
		t.Fatalf("locator mismatch: %+v", evaluation)
	}
	wantOrder := []string{"alpha", "mid", "zeta"}
	for i, check := range evaluation.Checks {
		if check.Name != wantOrder[i] {
			t.Fatalf("checks[%d] = %q, want sorted name %q", i, check.Name, wantOrder[i])
		}
	}
	if evaluation.Checks[1].WaiverReason == "" {
		t.Fatal("waived check must keep its waiver reason")
	}

	got, err := db.GetGateEvaluation(record.PublicID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil || len(got.Checks) != 3 || got.Checks[0].Name != "alpha" {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}

func TestSaveGateEvaluationIdempotentForIdenticalChecks(t *testing.T) {
	db := openTestStore(t)
	record := seedRecordForGate(t, db, "prod", "1.0.0", "pending")

	first, created, err := db.SaveGateEvaluation(record.PublicID, gateChecks("build", "pending", "tests", "passed"))
	if err != nil || !created {
		t.Fatalf("first save = %v, created %v", err, created)
	}
	// Same content, different request order.
	again, createdAgain, err := db.SaveGateEvaluation(record.PublicID, gateChecks("tests", "passed", "build", "pending"))
	if err != nil {
		t.Fatalf("identical resubmit: %v", err)
	}
	if createdAgain {
		t.Fatal("identical resubmit must not report created")
	}
	if again.GateStatus != first.GateStatus || !gateChecksEqual(again.Checks, first.Checks) {
		t.Fatalf("idempotent snapshot differs: %+v vs %+v", again, first)
	}
}

func TestSaveGateEvaluationConflictForDifferentChecks(t *testing.T) {
	db := openTestStore(t)
	record := seedRecordForGate(t, db, "prod", "1.0.0", "allowed")
	if _, _, err := db.SaveGateEvaluation(record.PublicID, gateChecks("build", "passed")); err != nil {
		t.Fatalf("first save: %v", err)
	}
	_, _, err := db.SaveGateEvaluation(record.PublicID, gateChecks("build", "passed", "smoke", "passed"))
	var conflict *ErrGateEvaluationConflict
	if !errors.As(err, &conflict) {
		t.Fatalf("different resubmit err = %v, want ErrGateEvaluationConflict", err)
	}
}

func TestSaveGateEvaluationStatusMismatch(t *testing.T) {
	db := openTestStore(t)
	record := seedRecordForGate(t, db, "prod", "1.0.0", "allowed")
	_, _, err := db.SaveGateEvaluation(record.PublicID, gateChecks("build", "pending"))
	var mismatch *ErrGateStatusMismatch
	if !errors.As(err, &mismatch) {
		t.Fatalf("err = %v, want ErrGateStatusMismatch", err)
	}
	if mismatch.Derived != "pending" || mismatch.Recorded != "allowed" {
		t.Fatalf("mismatch = %+v", mismatch)
	}
	// A mismatch write leaves no snapshot behind.
	got, err := db.GetGateEvaluation(record.PublicID)
	if err != nil {
		t.Fatalf("get after mismatch: %v", err)
	}
	if got != nil {
		t.Fatalf("mismatch must not store a snapshot: %+v", got)
	}
}

func TestSaveGateEvaluationUnknownRecord(t *testing.T) {
	db := openTestStore(t)
	_, _, err := db.SaveGateEvaluation("rel_missing", gateChecks("build", "passed"))
	var missing *ErrReleaseRecordMissing
	if !errors.As(err, &missing) {
		t.Fatalf("err = %v, want ErrReleaseRecordMissing", err)
	}
}

func TestDeriveGateStatusPrecedence(t *testing.T) {
	cases := []struct {
		statuses []string
		want     string
	}{
		{[]string{"passed", "passed"}, "allowed"},
		{[]string{"passed", "waived"}, "allowed"},
		{[]string{"pending", "passed"}, "pending"},
		{[]string{"failed", "pending"}, "blocked"},
		{[]string{"passed", "failed", "pending"}, "blocked"},
		{[]string{"waived", "pending", "failed"}, "blocked"},
	}
	for _, tc := range cases {
		checks := make([]GateCheck, len(tc.statuses))
		for i, status := range tc.statuses {
			checks[i] = GateCheck{Name: "c" + string(rune('a'+i)), Status: status, Evidence: "e"}
		}
		if got := DeriveGateStatus(checks); got != tc.want {
			t.Fatalf("derive %v = %q, want %q", tc.statuses, got, tc.want)
		}
	}
}

func TestSaveGateEvaluationRollsBackOnFault(t *testing.T) {
	db := openTestStore(t)
	record := seedRecordForGate(t, db, "prod", "1.0.0", "allowed")

	fired := false
	restore := SetGateEvaluationInsertFaultHookForTest(func(publicID string, recordID int64) error {
		if !fired {
			fired = true
			return errors.New("disk on fire")
		}
		return nil
	})
	defer restore()

	if _, _, err := db.SaveGateEvaluation(record.PublicID, gateChecks("build", "passed")); err == nil {
		t.Fatal("faulted save must fail")
	}
	var count int
	if err := db.db.QueryRow(`SELECT count(*) FROM release_gate_evaluations`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("rolled back save left %d snapshot rows", count)
	}
	if _, _, err := db.SaveGateEvaluation(record.PublicID, gateChecks("build", "passed")); err != nil {
		t.Fatalf("retry after rollback must succeed: %v", err)
	}
}

func TestListGateEvaluationsPageFiltersAndOrdersNewestFirst(t *testing.T) {
	db := openTestStore(t)
	recordA := seedRecordForGate(t, db, "prod", "1.0.0", "blocked")
	recordB := seedRecordForGate(t, db, "prod", "1.1.0", "allowed")
	recordC := seedRecordForGate(t, db, "staging", "2.0.0", "pending")
	if err := db.SetRecordedAtForTest(recordA.ID, "2026-10-01T08:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetRecordedAtForTest(recordB.ID, "2026-10-01T09:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetRecordedAtForTest(recordC.ID, "2026-10-01T10:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.SaveGateEvaluation(recordA.PublicID, gateChecks("build", "failed")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.SaveGateEvaluation(recordB.PublicID, gateChecks("build", "passed", "smoke", "waived")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.SaveGateEvaluation(recordC.PublicID, gateChecks("build", "pending")); err != nil {
		t.Fatal(err)
	}

	all, err := db.ListGateEvaluationsPage(GateEvaluationFilter{}, "", 0, 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("list len = %d, want 3", len(all))
	}
	if all[0].PublicID != recordC.PublicID || all[2].PublicID != recordA.PublicID {
		t.Fatalf("order = %s, %s, %s; want C, B, A newest first",
			all[0].PublicID, all[1].PublicID, all[2].PublicID)
	}

	prod, err := db.ListGateEvaluationsPage(GateEvaluationFilter{Environment: "prod"}, "", 0, 100)
	if err != nil || len(prod) != 2 {
		t.Fatalf("prod filter = %v, len %d", err, len(prod))
	}
	blocked, err := db.ListGateEvaluationsPage(GateEvaluationFilter{GateStatus: "blocked"}, "", 0, 100)
	if err != nil || len(blocked) != 1 || blocked[0].PublicID != recordA.PublicID {
		t.Fatalf("gate_status filter = %v, %+v", err, blocked)
	}
	failedChecks, err := db.ListGateEvaluationsPage(GateEvaluationFilter{CheckStatus: "failed"}, "", 0, 100)
	if err != nil || len(failedChecks) != 1 || failedChecks[0].PublicID != recordA.PublicID {
		t.Fatalf("check_status filter = %v, %+v", err, failedChecks)
	}
	waived, err := db.ListGateEvaluationsPage(GateEvaluationFilter{CheckStatus: "waived"}, "", 0, 100)
	if err != nil || len(waived) != 1 || waived[0].PublicID != recordB.PublicID {
		t.Fatalf("waived filter = %v, %+v", err, waived)
	}
	versionFilter, err := db.ListGateEvaluationsPage(GateEvaluationFilter{Version: "2.0.0"}, "", 0, 100)
	if err != nil || len(versionFilter) != 1 || versionFilter[0].PublicID != recordC.PublicID {
		t.Fatalf("version filter = %v, %+v", err, versionFilter)
	}

	// Keyset: one per page walks all three in order with no repeats.
	var walked []string
	var anchorAt string
	var anchorID int64
	for page := 0; page < 5; page++ {
		items, err := db.ListGateEvaluationsPage(GateEvaluationFilter{}, anchorAt, anchorID, 1)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		if len(items) == 0 {
			break
		}
		if page < 2 && len(items) != 2 {
			t.Fatalf("page %d returned %d items, want limit+1 = 2", page, len(items))
		}
		items = items[:1]
		walked = append(walked, items[0].PublicID)
		anchorAt = items[0].RecordedAt
		anchorID = items[0].RecordID
	}
	want := []string{recordC.PublicID, recordB.PublicID, recordA.PublicID}
	if len(walked) != 3 {
		t.Fatalf("walk = %v, want %v", walked, want)
	}
	for i := range want {
		if walked[i] != want[i] {
			t.Fatalf("walk = %v, want %v", walked, want)
		}
	}
}

func TestGetGateEvaluationMissing(t *testing.T) {
	db := openTestStore(t)
	got, err := db.GetGateEvaluation("rel_none")
	if err != nil {
		t.Fatalf("get missing: %v", err)
	}
	if got != nil {
		t.Fatalf("missing snapshot = %+v, want nil", got)
	}
}
