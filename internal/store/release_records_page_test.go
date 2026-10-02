package store

import "testing"

func TestListReleaseRecordsPageKeysetAndFilters(t *testing.T) {
	db := openTestStore(t)
	for _, env := range []string{"prod", "dev"} {
		if _, err := db.EnsureEnvironment(&TrackedEnvironment{Environment: env}); err != nil {
			t.Fatal(err)
		}
	}
	var prodIDs []int64
	for i := 0; i < 4; i++ {
		rec := sampleRecord("prod", "1.0."+string(rune('0'+i)), "allowed", "rb", entries("a")...)
		rec.BatchID = "b-1"
		if err := db.InsertReleaseRecord(rec); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
		prodIDs = append(prodIDs, rec.ID)
	}
	blocked := sampleRecord("prod", "2.0.0", "blocked", "rb", entries("b")...)
	if err := db.InsertReleaseRecord(blocked); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertReleaseRecord(sampleRecord("dev", "3.0.0", "allowed", "rb", entries("c")...)); err != nil {
		t.Fatal(err)
	}

	first, err := db.ListReleaseRecordsPage(ReleaseRecordFilter{Environment: "prod"}, "", 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 3 {
		t.Fatalf("first page fetches limit+1 = 3, got %d", len(first))
	}
	if first[0].ID != blocked.ID {
		t.Fatalf("newest first = %d, want %d", first[0].ID, blocked.ID)
	}
	if len(first[0].Changes) != 1 || first[0].Changes[0].Title != "b" {
		t.Fatalf("page records must keep their change entries: %+v", first[0].Changes)
	}

	second, err := db.ListReleaseRecordsPage(
		ReleaseRecordFilter{Environment: "prod"}, first[1].RecordedAt, first[1].ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int64]bool{}
	for _, rec := range append(first[:2], second...) {
		if seen[rec.ID] {
			t.Fatalf("record %d repeated across pages", rec.ID)
		}
		seen[rec.ID] = true
	}
	for _, id := range prodIDs {
		if !seen[id] {
			t.Fatalf("prod record %d missing from the walk", id)
		}
	}
	if seen[0] {
		t.Fatal("internal zero id leaked")
	}

	// Filters apply to paged queries exactly like ListReleaseRecords.
	batched, err := db.ListReleaseRecordsPage(ReleaseRecordFilter{BatchID: "b-1"}, "", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range batched {
		if rec.BatchID != "b-1" {
			t.Fatalf("batch filter leaked %d (%q)", rec.ID, rec.BatchID)
		}
	}
	if len(batched) != 4 {
		t.Fatalf("batched page = %d rows, want 4", len(batched))
	}
}

func TestResolveReleaseRecordPosition(t *testing.T) {
	db := openTestStore(t)
	if _, err := db.EnsureEnvironment(&TrackedEnvironment{Environment: "prod"}); err != nil {
		t.Fatal(err)
	}
	rec := sampleRecord("prod", "1.0.0", "allowed", "rb")
	if err := db.InsertReleaseRecord(rec); err != nil {
		t.Fatal(err)
	}
	at, id, ok, err := db.ResolveReleaseRecordPosition(rec.PublicID)
	if err != nil || !ok {
		t.Fatalf("resolve: ok=%v err=%v", ok, err)
	}
	if id != rec.ID || at != rec.RecordedAt {
		t.Fatalf("position = %d@%s, want %d@%s", id, at, rec.ID, rec.RecordedAt)
	}
	if _, _, ok, err := db.ResolveReleaseRecordPosition("rel_missing"); err != nil || ok {
		t.Fatalf("missing public id: ok=%v err=%v", ok, err)
	}
}
