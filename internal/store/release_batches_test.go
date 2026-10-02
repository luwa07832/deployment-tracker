package store

import (
	"reflect"
	"testing"
)

// seedReleaseBatches writes two batches plus one unbatched record:
//
//	b-zeta: dev (2 records, 3 changes) and prod (1 record, 1 change)
//	b-alpha: dev (1 record, 1 change)
//	unbatched prod 9.0.0 (never a batch)
func seedReleaseBatches(t *testing.T, db *Store) {
	t.Helper()
	for _, env := range []string{"prod", "dev"} {
		if _, err := db.EnsureEnvironment(&TrackedEnvironment{Environment: env}); err != nil {
			t.Fatal(err)
		}
	}
	batches := []struct {
		batch, env, version, gate string
		changes                   []string
	}{
		{"b-zeta", "dev", "1.0.0", "allowed", []string{"a", "b"}},
		{"b-zeta", "dev", "1.1.0", "blocked", []string{"c"}},
		{"b-zeta", "prod", "1.1.0", "pending", []string{"d"}},
		{"b-alpha", "dev", "2.0.0", "allowed", []string{"e"}},
		{"", "prod", "9.0.0", "allowed", []string{"u"}},
	}
	for _, item := range batches {
		rec := sampleRecord(item.env, item.version, item.gate, "rb", entries(item.changes...)...)
		rec.BatchID = item.batch
		if err := db.InsertReleaseRecord(rec); err != nil {
			t.Fatalf("insert %+v: %v", item, err)
		}
	}
	times := map[string]string{
		"1.0.0": "2026-10-01T08:00:00Z",
		"1.1.0": "2026-10-02T08:00:00Z",
		"2.0.0": "2026-10-03T08:00:00Z",
	}
	for version, recordedAt := range times {
		records, err := db.ListReleaseRecords(ReleaseRecordFilter{Version: version})
		if err != nil {
			t.Fatal(err)
		}
		for i := range records {
			if err := db.SetRecordedAtForTest(records[i].ID, recordedAt); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func batchSummaryByID(t *testing.T, summaries []ReleaseBatchSummary) map[string]ReleaseBatchSummary {
	t.Helper()
	byID := map[string]ReleaseBatchSummary{}
	for _, summary := range summaries {
		byID[summary.BatchID] = summary
	}
	return byID
}

func TestListReleaseBatchesPageSummariesAndOrder(t *testing.T) {
	db := openTestStore(t)
	seedReleaseBatches(t, db)

	page, err := db.ListReleaseBatchesPage(ReleaseBatchFilter{}, "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 2 {
		t.Fatalf("two batches qualify (unbatched row excluded), got %d: %+v", len(page), page)
	}
	if page[0].BatchID != "b-alpha" || page[1].BatchID != "b-zeta" {
		t.Fatalf("order = %s, %s; want newest last_recorded_at first", page[0].BatchID, page[1].BatchID)
	}
	zeta := page[1]
	if zeta.ReleaseCount != 3 {
		t.Fatalf("release_count = %d, want 3", zeta.ReleaseCount)
	}
	if zeta.ChangeCount != 4 {
		t.Fatalf("change_count = %d, want 4 (all records, not only a filter match)", zeta.ChangeCount)
	}
	if zeta.GateAllowed != 1 || zeta.GateBlocked != 1 || zeta.GatePending != 1 {
		t.Fatalf("gate counts = %d/%d/%d, want 1/1/1", zeta.GateAllowed, zeta.GateBlocked, zeta.GatePending)
	}
	if zeta.LastRecordedAt == "" {
		t.Fatal("last_recorded_at must be populated")
	}
	if zeta.LastRecordedAt >= page[0].LastRecordedAt {
		t.Fatalf("ordering must follow last_recorded_at desc: %s <= %s",
			zeta.LastRecordedAt, page[0].LastRecordedAt)
	}
}

func TestListReleaseBatchesQualifiesByRecordFilterButSummarizesAll(t *testing.T) {
	db := openTestStore(t)
	seedReleaseBatches(t, db)

	page, err := db.ListReleaseBatchesPage(ReleaseBatchFilter{GateStatus: "blocked"}, "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 1 || page[0].BatchID != "b-zeta" {
		t.Fatalf("only b-zeta owns a blocked record, got %+v", page)
	}
	if page[0].ReleaseCount != 3 || page[0].ChangeCount != 4 {
		t.Fatalf("summary must cover every batch record: %+v", page[0])
	}

	versioned, err := db.ListReleaseBatchesPage(ReleaseBatchFilter{Version: "2.0.0"}, "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(versioned) != 1 || versioned[0].BatchID != "b-alpha" {
		t.Fatalf("version filter = %+v, want b-alpha only", versioned)
	}

	inRange, err := db.ListReleaseBatchesPage(
		ReleaseBatchFilter{From: "2026-10-03T00:00:00Z", To: "2026-10-03T23:59:59Z"},
		"", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(inRange) != 1 || inRange[0].BatchID != "b-alpha" {
		t.Fatalf("time range = %+v, want b-alpha only", inRange)
	}

	empty, err := db.ListReleaseBatchesPage(ReleaseBatchFilter{Environment: "prod", GateStatus: "blocked"}, "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Fatalf("AND semantics must exclude b-zeta (no blocked prod record), got %+v", empty)
	}
}

func TestListReleaseBatchesKeysetWalksEveryBatchOnce(t *testing.T) {
	db := openTestStore(t)
	seedReleaseBatches(t, db)

	first, err := db.ListReleaseBatchesPage(ReleaseBatchFilter{}, "", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 {
		t.Fatalf("limit+1 = 2 rows fetched, got %d", len(first))
	}
	second, err := db.ListReleaseBatchesPage(
		ReleaseBatchFilter{}, first[0].LastRecordedAt, first[0].BatchID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if first[0].BatchID != "b-alpha" {
		t.Fatalf("first batch = %s, want newest b-alpha", first[0].BatchID)
	}
	if len(second) != 1 || second[0].BatchID != "b-zeta" {
		t.Fatalf("second page = %+v, want b-zeta alone", second)
	}
	third, err := db.ListReleaseBatchesPage(
		ReleaseBatchFilter{}, second[0].LastRecordedAt, second[0].BatchID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(third) != 0 {
		t.Fatalf("page after the last batch must be empty, got %+v", third)
	}
}

func TestListReleaseBatchesSameTimestampOrdersByBatchIDDesc(t *testing.T) {
	db := openTestStore(t)
	for _, env := range []string{"prod"} {
		if _, err := db.EnsureEnvironment(&TrackedEnvironment{Environment: env}); err != nil {
			t.Fatal(err)
		}
	}
	for _, batch := range []string{"b-a", "b-b", "b-c"} {
		rec := sampleRecord("prod", "v-"+batch, "allowed", "rb", entries("x")...)
		rec.BatchID = batch
		if err := db.InsertReleaseRecord(rec); err != nil {
			t.Fatal(err)
		}
		if err := db.SetRecordedAtForTest(rec.ID, "2026-10-01T08:00:00Z"); err != nil {
			t.Fatal(err)
		}
	}
	page, err := db.ListReleaseBatchesPage(ReleaseBatchFilter{}, "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{page[0].BatchID, page[1].BatchID, page[2].BatchID}; !reflect.DeepEqual(got, []string{"b-c", "b-b", "b-a"}) {
		t.Fatalf("tie order = %v, want batch_id desc", got)
	}
}

func TestGetReleaseBatchSummaryAndEnvironments(t *testing.T) {
	db := openTestStore(t)
	seedReleaseBatches(t, db)

	summary, err := db.GetReleaseBatchSummary("b-zeta")
	if err != nil {
		t.Fatal(err)
	}
	if summary == nil {
		t.Fatal("b-zeta must exist")
	}
	if summary.ReleaseCount != 3 || summary.ChangeCount != 4 {
		t.Fatalf("summary = %+v", summary)
	}
	missing, err := db.GetReleaseBatchSummary("nope")
	if err != nil || missing != nil {
		t.Fatalf("missing batch = %+v, %v", missing, err)
	}
	unbatched, err := db.GetReleaseBatchSummary("")
	if err != nil || unbatched != nil {
		t.Fatalf("empty batch id must not match unbatched rows: %+v, %v", unbatched, err)
	}

	environments, err := db.ListReleaseBatchEnvironments("b-zeta")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(environments, []string{"dev", "prod"}) {
		t.Fatalf("environments = %v, want sorted unique [dev prod]", environments)
	}
}

func TestListReleaseBatchRecordsChronological(t *testing.T) {
	db := openTestStore(t)
	for _, env := range []string{"dev", "prod"} {
		if _, err := db.EnsureEnvironment(&TrackedEnvironment{Environment: env}); err != nil {
			t.Fatal(err)
		}
	}
	rec := sampleRecord("dev", "1.0.0", "allowed", "rb", entries("a")...)
	rec.BatchID = "b-1"
	if err := db.InsertReleaseRecord(rec); err != nil {
		t.Fatal(err)
	}
	if err := db.SetRecordedAtForTest(rec.ID, "2026-10-02T08:00:00Z"); err != nil {
		t.Fatal(err)
	}
	older := sampleRecord("prod", "2.0.0", "blocked", "rb", entries("b", "c")...)
	older.BatchID = "b-1"
	if err := db.InsertReleaseRecord(older); err != nil {
		t.Fatal(err)
	}
	if err := db.SetRecordedAtForTest(older.ID, "2026-10-01T08:00:00Z"); err != nil {
		t.Fatal(err)
	}

	records, err := db.ListReleaseBatchRecords("b-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[0].Version != "2.0.0" || records[1].Version != "1.0.0" {
		t.Fatalf("chronological order = %+v", records)
	}
	if len(records[0].Changes) != 2 || records[0].Changes[0].Title != "b" {
		t.Fatalf("change entries must survive chronological listing: %+v", records[0].Changes)
	}
}
