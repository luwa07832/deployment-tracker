package store

import "testing"

func insertBatched(t *testing.T, db *Store, environment, version, batchID, gate string, titles ...string) {
	t.Helper()
	record := sampleRecord(environment, version, gate, "rb:0.9.0", entries(titles...)...)
	record.BatchID = batchID
	if err := db.InsertReleaseRecord(record); err != nil {
		t.Fatalf("insert %s/%s/%s: %v", batchID, environment, version, err)
	}
}

func TestListReleaseBatchesAggregatesEveryRecordOfMatchingBatches(t *testing.T) {
	db := openTestStore(t)
	insertBatched(t, db, "prod", "1.0.0", "b-1", "allowed", "alpha")
	insertBatched(t, db, "staging", "1.0.0", "b-1", "blocked", "beta")
	insertBatched(t, db, "dev", "1.0.0", "b-2", "allowed", "alpha", "gamma")
	insertBatched(t, db, "prod", "1.1.0", "b-2", "pending")
	// Unbatched rows never form a batch.
	if err := db.InsertReleaseRecord(sampleRecord("prod", "9.0.0", "allowed", "rb:0", entries("solo")...)); err != nil {
		t.Fatal(err)
	}

	page, err := db.ListReleaseBatches(ReleaseBatchFilter{}, "", "", 20)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page) != 2 {
		t.Fatalf("batch count = %d, want 2: %+v", len(page), page)
	}
	if page[0].BatchID != "b-2" || page[1].BatchID != "b-1" {
		t.Fatalf("order = %s, %s (newest first)", page[0].BatchID, page[1].BatchID)
	}
	b2 := page[0]
	if b2.ReleaseCount != 2 || b2.ChangeCount != 2 {
		t.Fatalf("b-2 counts = %+v", b2)
	}
	if b2.GateCounts.Allowed != 1 || b2.GateCounts.Pending != 1 || b2.GateCounts.Blocked != 0 {
		t.Fatalf("b-2 gate counts = %+v", b2.GateCounts)
	}
	if len(b2.Environments) != 2 || b2.Environments[0] != "dev" || b2.Environments[1] != "prod" {
		t.Fatalf("b-2 environments = %v", b2.Environments)
	}
	if b2.LastRecordedAt == "" {
		t.Fatal("b-2 last recorded at empty")
	}

	// A filter selecting b-1 through its staging record still aggregates the
	// whole batch, and a future bound excludes everything deterministically.
	page, err = db.ListReleaseBatches(ReleaseBatchFilter{
		Environment: "staging",
		GateStatus:  "blocked",
	}, "", "", 20)
	if err != nil {
		t.Fatalf("filtered list: %v", err)
	}
	if len(page) != 1 || page[0].BatchID != "b-1" {
		t.Fatalf("filtered page = %+v", page)
	}
	if page[0].ReleaseCount != 2 || page[0].ChangeCount != 2 ||
		page[0].GateCounts.Allowed != 1 || page[0].GateCounts.Blocked != 1 {
		t.Fatalf("summary must cover the whole batch: %+v", page[0])
	}

	future, err := db.ListReleaseBatches(ReleaseBatchFilter{From: "3000-01-01T00:00:00Z"}, "", "", 20)
	if err != nil {
		t.Fatalf("future list: %v", err)
	}
	if len(future) != 0 {
		t.Fatalf("future bound must exclude every batch: %+v", future)
	}
}

func TestListReleaseBatchesKeysetAnchor(t *testing.T) {
	db := openTestStore(t)
	insertBatched(t, db, "prod", "1.0.0", "b-a", "allowed")
	insertBatched(t, db, "prod", "2.0.0", "b-b", "allowed")
	insertBatched(t, db, "prod", "3.0.0", "b-c", "allowed")

	page, err := db.ListReleaseBatches(ReleaseBatchFilter{}, "", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 2 {
		t.Fatalf("first page must return limit+1 rows: %d", len(page))
	}
	last := page[0]
	rest, err := db.ListReleaseBatches(
		ReleaseBatchFilter{}, last.LastRecordedAt, last.BatchID, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 2 {
		t.Fatalf("rest count = %d, want 2: %+v", len(rest), rest)
	}
	seen := map[string]bool{last.BatchID: true}
	for _, summary := range rest {
		if seen[summary.BatchID] {
			t.Fatalf("batch %s returned twice", summary.BatchID)
		}
		if summary.LastRecordedAt > last.LastRecordedAt {
			t.Fatalf("anchor leaked a newer batch: %s", summary.BatchID)
		}
		seen[summary.BatchID] = true
	}
}

func TestGetReleaseBatchReturnsAllRecordsAndSummary(t *testing.T) {
	db := openTestStore(t)
	insertBatched(t, db, "prod", "1.0.0", "b-1", "allowed", "alpha")
	insertBatched(t, db, "staging", "1.0.0", "b-1", "pending", "beta", "gamma")

	summary, err := db.GetReleaseBatch("b-1")
	if err != nil {
		t.Fatal(err)
	}
	if summary == nil {
		t.Fatal("b-1 must exist")
	}
	if summary.ReleaseCount != 2 || summary.ChangeCount != 3 {
		t.Fatalf("counts = %+v", summary)
	}
	if summary.GateCounts.Allowed != 1 || summary.GateCounts.Pending != 1 || summary.GateCounts.Blocked != 0 {
		t.Fatalf("gate counts = %+v", summary.GateCounts)
	}
	if len(summary.Releases) != 2 {
		t.Fatalf("releases = %d", len(summary.Releases))
	}
	first, second := summary.Releases[0], summary.Releases[1]
	if first.RecordedAt > second.RecordedAt {
		t.Fatalf("releases must be oldest first: %s then %s", first.RecordedAt, second.RecordedAt)
	}
	if first.RecordedAt == second.RecordedAt && first.ID < second.ID {
		t.Fatalf("same-second ties must order id descending: %d then %d", first.ID, second.ID)
	}
	if len(first.Changes) != 2 || first.Changes[0].Title != "beta" || first.Changes[1].Title != "gamma" {
		t.Fatalf("first record changes must be complete: %+v", first.Changes)
	}
	if len(second.Changes) != 1 || second.Changes[0].Title != "alpha" {
		t.Fatalf("second record changes must be complete: %+v", second.Changes)
	}

	missing, err := db.GetReleaseBatch("nope")
	if err != nil || missing != nil {
		t.Fatalf("missing batch = %+v, %v", missing, err)
	}
	empty, err := db.GetReleaseBatch("")
	if err != nil || empty != nil {
		t.Fatalf("empty batch id = %+v, %v", empty, err)
	}
}
