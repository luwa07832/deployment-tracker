package store

import "testing"

func changeRecord(environment, version, batch, gate, rollback string, changes ...ChangeEntry) *ReleaseRecord {
	record := sampleRecord(environment, version, gate, rollback, changes...)
	record.BatchID = batch
	return record
}

func seqEntry(sequence int, category, title, description string) ChangeEntry {
	return ChangeEntry{Sequence: sequence, Category: category, Title: title, Description: description}
}

func mustInsertRecord(t *testing.T, db *Store, record *ReleaseRecord, recordedAt string) *ReleaseRecord {
	t.Helper()
	if err := db.InsertReleaseRecord(record); err != nil {
		t.Fatalf("insert %s %s: %v", record.Environment, record.Version, err)
	}
	if recordedAt != "" {
		if err := db.SetRecordedAtForTest(record.ID, recordedAt); err != nil {
			t.Fatalf("pin recorded_at: %v", err)
		}
		record.RecordedAt = recordedAt
	}
	return record
}

func itemKeys(items []ChangeEntryItem) []string {
	keys := make([]string, 0, len(items))
	for _, item := range items {
		keys = append(keys, item.Environment+"/"+item.Version+"#"+item.Entry.Title)
	}
	return keys
}

func TestListChangeEntriesOrdersReleasesDescAndEntriesAsc(t *testing.T) {
	db := openTestStore(t)
	for _, env := range []string{"prod"} {
		if _, err := db.EnsureEnvironment(&TrackedEnvironment{Environment: env}); err != nil {
			t.Fatal(err)
		}
	}
	// r1 is oldest with two entries; r2 is one second later with one entry.
	r1 := mustInsertRecord(t, db, changeRecord("prod", "1.0.0", "", "allowed", "rb:1",
		seqEntry(2, "fix", "zeta", "d"), seqEntry(1, "feature", "alpha", "d")),
		"2026-10-01T08:00:00Z")
	r2 := mustInsertRecord(t, db, changeRecord("prod", "1.1.0", "", "blocked", "rb:2",
		seqEntry(1, "feature", "beta", "d")),
		"2026-10-01T09:00:00Z")

	items, err := db.ListChangeEntries(ChangeEntryFilter{Environment: "prod"}, ChangeEntryItem{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if got := itemKeys(items); len(got) != 3 ||
		got[0] != "prod/1.1.0#beta" || got[1] != "prod/1.0.0#alpha" || got[2] != "prod/1.0.0#zeta" {
		t.Fatalf("order = %v", got)
	}
	if items[0].PublicID != r2.PublicID || items[1].PublicID != r1.PublicID {
		t.Fatalf("items must carry owning release public ids: %+v", items)
	}
	if items[0].GateStatus != "blocked" || items[1].RollbackPoint != "rb:1" {
		t.Fatalf("owning release fields missing: %+v", items)
	}
}

func TestListChangeEntriesSameSecondUsesWriteOrderDesc(t *testing.T) {
	db := openTestStore(t)
	if _, err := db.EnsureEnvironment(&TrackedEnvironment{Environment: "prod"}); err != nil {
		t.Fatal(err)
	}
	at := "2026-10-01T08:00:00Z"
	r1 := mustInsertRecord(t, db, changeRecord("prod", "1.0.0", "", "allowed", "rb", seqEntry(1, "c", "first", "d")), at)
	r2 := mustInsertRecord(t, db, changeRecord("prod", "1.0.1", "", "allowed", "rb", seqEntry(1, "c", "second", "d")), at)
	r3 := mustInsertRecord(t, db, changeRecord("prod", "1.0.2", "", "allowed", "rb", seqEntry(1, "c", "third", "d")), at)

	items, err := db.ListChangeEntries(ChangeEntryFilter{}, ChangeEntryItem{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("items = %d, want 3", len(items))
	}
	want := []string{r3.PublicID, r2.PublicID, r1.PublicID}
	for i, id := range want {
		if items[i].PublicID != id {
			t.Fatalf("same-second order = %v, want newest write first", itemKeys(items))
		}
	}
}

func TestListChangeEntriesAppliesAllFilters(t *testing.T) {
	db := openTestStore(t)
	for _, env := range []string{"prod", "dev"} {
		if _, err := db.EnsureEnvironment(&TrackedEnvironment{Environment: env}); err != nil {
			t.Fatal(err)
		}
	}
	mustInsertRecord(t, db, changeRecord("prod", "1.0.0", "b-1", "allowed", "rb",
		seqEntry(1, "feature", "alpha", "d")), "2026-10-01T08:00:00Z")
	mustInsertRecord(t, db, changeRecord("prod", "2.0.0", "b-2", "blocked", "rb",
		seqEntry(1, "fix", "beta", "d")), "2026-10-02T08:00:00Z")
	mustInsertRecord(t, db, changeRecord("dev", "1.0.0", "b-1", "allowed", "rb",
		seqEntry(1, "feature", "alpha", "d")), "2026-10-03T08:00:00Z")

	cases := []struct {
		name   string
		filter ChangeEntryFilter
		want   []string
	}{
		{"environment", ChangeEntryFilter{Environment: "dev"}, []string{"dev/1.0.0#alpha"}},
		{"version", ChangeEntryFilter{Version: "1.0.0"}, []string{"dev/1.0.0#alpha", "prod/1.0.0#alpha"}},
		{"batch_id", ChangeEntryFilter{BatchID: "b-1"}, []string{"dev/1.0.0#alpha", "prod/1.0.0#alpha"}},
		{"category", ChangeEntryFilter{Category: "fix"}, []string{"prod/2.0.0#beta"}},
		{"title", ChangeEntryFilter{Title: "alpha"}, []string{"dev/1.0.0#alpha", "prod/1.0.0#alpha"}},
		{"gate_status", ChangeEntryFilter{GateStatus: "blocked"}, []string{"prod/2.0.0#beta"}},
		{"from", ChangeEntryFilter{From: "2026-10-02T00:00:00Z"}, []string{"dev/1.0.0#alpha", "prod/2.0.0#beta"}},
		{"to", ChangeEntryFilter{To: "2026-10-01T23:59:59Z"}, []string{"prod/1.0.0#alpha"}},
		{"combined", ChangeEntryFilter{
			Environment: "prod", Version: "2.0.0", BatchID: "b-2", Category: "fix",
			Title: "beta", GateStatus: "blocked",
			From: "2026-10-02T00:00:00Z", To: "2026-10-02T23:59:59Z",
		}, []string{"prod/2.0.0#beta"}},
		{"no match", ChangeEntryFilter{Environment: "prod", Title: "missing"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			items, err := db.ListChangeEntries(tc.filter, ChangeEntryItem{}, 100)
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == nil {
				if len(items) != 0 {
					t.Fatalf("items = %v, want empty", itemKeys(items))
				}
				return
			}
			if got := itemKeys(items); !equalStrings(got, tc.want) {
				t.Fatalf("items = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestListChangeEntriesKeysetPaginatesWithoutDuplicatesOrGaps(t *testing.T) {
	db := openTestStore(t)
	if _, err := db.EnsureEnvironment(&TrackedEnvironment{Environment: "prod"}); err != nil {
		t.Fatal(err)
	}
	// Two releases sharing one recorded second, three entries each.
	at := "2026-10-01T08:00:00Z"
	mustInsertRecord(t, db, changeRecord("prod", "1.0.0", "", "allowed", "rb",
		seqEntry(1, "c", "a", "d"), seqEntry(2, "c", "m", "d"), seqEntry(3, "c", "z", "d")), at)
	second := mustInsertRecord(t, db, changeRecord("prod", "1.0.1", "", "allowed", "rb",
		seqEntry(1, "c", "b", "d"), seqEntry(2, "c", "n", "d"), seqEntry(3, "c", "y", "d")), at)

	anchor := ChangeEntryItem{}
	var seen []ChangeEntryItem
	for page := 0; page < 10; page++ {
		items, err := db.ListChangeEntries(ChangeEntryFilter{Environment: "prod"}, anchor, 2)
		if err != nil {
			t.Fatal(err)
		}
		hasMore := len(items) == 3
		if hasMore {
			items = items[:2]
		}
		seen = append(seen, items...)
		if !hasMore {
			break
		}
		last := items[len(items)-1]
		anchor = ChangeEntryItem{
			RecordedAt: last.RecordedAt, RecordID: last.RecordID,
			Entry: ChangeEntry{Sequence: last.Entry.Sequence, Title: last.Entry.Title},
		}
	}
	if len(seen) != 6 {
		t.Fatalf("paged items = %d, want 6", len(seen))
	}
	wantOrder := []string{
		"prod/1.0.1#b", "prod/1.0.1#n", "prod/1.0.1#y",
		"prod/1.0.0#a", "prod/1.0.0#m", "prod/1.0.0#z",
	}
	if got := itemKeys(seen); !equalStrings(got, wantOrder) {
		t.Fatalf("paged order = %v, want %v (newer release %s first)", got, wantOrder, second.PublicID)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestListChangeEntriesPaginatesWithinOneReleaseAcrossPages(t *testing.T) {
	db := openTestStore(t)
	if _, err := db.EnsureEnvironment(&TrackedEnvironment{Environment: "prod"}); err != nil {
		t.Fatal(err)
	}
	mustInsertRecord(t, db, changeRecord("prod", "1.0.0", "", "allowed", "rb",
		seqEntry(1, "c", "a", "d"), seqEntry(2, "c", "b", "d"),
		seqEntry(3, "c", "c", "d"), seqEntry(4, "c", "d", "d")), "2026-10-01T08:00:00Z")

	anchor := ChangeEntryItem{}
	var got []string
	for {
		items, err := db.ListChangeEntries(ChangeEntryFilter{}, anchor, 2)
		if err != nil {
			t.Fatal(err)
		}
		hasMore := len(items) == 3
		if hasMore {
			items = items[:2]
		}
		for _, item := range items {
			got = append(got, item.Entry.Title)
		}
		if !hasMore {
			break
		}
		last := items[len(items)-1]
		anchor = ChangeEntryItem{
			RecordedAt: last.RecordedAt, RecordID: last.RecordID,
			Entry: ChangeEntry{Sequence: last.Entry.Sequence, Title: last.Entry.Title},
		}
	}
	if !equalStrings(got, []string{"a", "b", "c", "d"}) {
		t.Fatalf("within-release pagination = %v", got)
	}
}

func TestListChangeEntriesTitleBreaksSequenceTie(t *testing.T) {
	db := openTestStore(t)
	if _, err := db.EnsureEnvironment(&TrackedEnvironment{Environment: "prod"}); err != nil {
		t.Fatal(err)
	}
	// Legacy-seeded rows may share a sequence; title ascending must order them.
	mustInsertRecord(t, db, changeRecord("prod", "1.0.0", "", "allowed", "rb",
		seqEntry(1, "c", "zulu", "d"), seqEntry(1, "c", "alpha", "d")),
		"2026-10-01T08:00:00Z")
	items, err := db.ListChangeEntries(ChangeEntryFilter{}, ChangeEntryItem{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].Entry.Title != "alpha" || items[1].Entry.Title != "zulu" {
		t.Fatalf("title tiebreak failed: %+v", items)
	}
}
