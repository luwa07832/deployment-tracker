package store

import "testing"

// The store never deduplicated change entries by title: records saved with
// repeated titles (pre-rule histories) must round-trip every entry in
// sequence order so the read paths can compare them item by item.
func TestReleaseRecordPreservesRepeatedTitleEntries(t *testing.T) {
	db := openTestStore(t)
	if _, err := db.EnsureEnvironment(&TrackedEnvironment{Environment: "prod"}); err != nil {
		t.Fatalf("ensure env: %v", err)
	}
	record := sampleRecord("prod", "1.0.0", "allowed", "0.9.0",
		ChangeEntry{Sequence: 1, Category: "feature", Title: "dup", Description: "first"},
		ChangeEntry{Sequence: 2, Category: "fix", Title: "dup", Description: "second"},
		ChangeEntry{Sequence: 3, Category: "chore", Title: "other", Description: "third"},
	)
	if err := db.InsertReleaseRecord(record); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, err := db.GetReleaseRecord(record.PublicID)
	if err != nil || got == nil {
		t.Fatalf("get: %v %v", got, err)
	}
	if len(got.Changes) != 3 {
		t.Fatalf("changes = %+v, want all three entries", got.Changes)
	}
	if got.Changes[0].Description != "first" || got.Changes[1].Description != "second" ||
		got.Changes[1].Title != "dup" || got.Changes[2].Title != "other" {
		t.Fatalf("change entries lost or reordered: %+v", got.Changes)
	}

	listed, err := db.ListReleaseRecords(ReleaseRecordFilter{Environment: "prod", Version: "1.0.0"})
	if err != nil || len(listed) != 1 || len(listed[0].Changes) != 3 {
		t.Fatalf("listed = %+v (err %v), want one record with three entries", listed, err)
	}
}
