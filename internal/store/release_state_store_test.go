package store

import "testing"

// backdateRecord stamps a stored record with a fixed recorded_at so cutoff
// queries can be exercised without racing the wall clock.
func backdateRecord(t *testing.T, db *Store, publicID, recordedAt string) {
	t.Helper()
	if _, err := db.db.Exec(
		`UPDATE release_records SET recorded_at = ? WHERE public_id = ?`,
		recordedAt, publicID,
	); err != nil {
		t.Fatalf("backdate %s: %v", publicID, err)
	}
}

func TestListLatestReleaseRecordsCutoffAndWriteOrder(t *testing.T) {
	db := openTestStore(t)
	v1 := sampleRecord("prod", "1.0.0", "allowed", "1.0.0", entries("a")...)
	v2 := sampleRecord("prod", "1.1.0", "blocked", "1.0.0", entries("a", "b")...)
	v3 := sampleRecord("prod", "1.2.0", "allowed", "1.1.0", entries("a", "b", "c")...)
	for _, record := range []*ReleaseRecord{v1, v2, v3} {
		if err := db.InsertReleaseRecord(record); err != nil {
			t.Fatalf("insert %s: %v", record.Version, err)
		}
	}
	backdateRecord(t, db, v1.PublicID, "2026-09-01T08:00:00Z")
	backdateRecord(t, db, v2.PublicID, "2026-09-02T08:00:00Z")
	// v3 keeps the later server timestamp, so it must sort newest.

	latest, err := db.ListLatestReleaseRecords("prod", "2999-01-01T00:00:00Z", 2)
	if err != nil {
		t.Fatalf("list latest: %v", err)
	}
	if len(latest) != 2 || latest[0].Version != "1.2.0" || latest[1].Version != "1.1.0" {
		t.Fatalf("latest = %v, want [1.2.0 1.1.0]", versionList(latest))
	}
	if len(latest[0].Changes) != 3 {
		t.Fatalf("newest record lost change entries: %d", len(latest[0].Changes))
	}

	atV2, err := db.ListLatestReleaseRecords("prod", "2026-09-02T08:00:00Z", 2)
	if err != nil {
		t.Fatalf("list at v2: %v", err)
	}
	if len(atV2) != 2 || atV2[0].Version != "1.1.0" || atV2[1].Version != "1.0.0" {
		t.Fatalf("cutoff latest = %v, want [1.1.0 1.0.0]", versionList(atV2))
	}

	beforeAny, err := db.ListLatestReleaseRecords("prod", "2000-01-01T00:00:00Z", 2)
	if err != nil {
		t.Fatalf("list before any: %v", err)
	}
	if len(beforeAny) != 0 {
		t.Fatalf("before-any rows = %v, want empty", versionList(beforeAny))
	}
}

func TestListLatestReleaseRecordsSameTimestampLaterWriterWins(t *testing.T) {
	db := openTestStore(t)
	v1 := sampleRecord("prod", "1.0.0", "allowed", "1.0.0", entries("a")...)
	v2 := sampleRecord("prod", "1.1.0", "blocked", "1.0.0", entries("a", "b")...)
	for _, record := range []*ReleaseRecord{v1, v2} {
		if err := db.InsertReleaseRecord(record); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	// Identical recorded_at: the later write (higher internal id) is current.
	stamp := "2026-09-01T08:00:00Z"
	backdateRecord(t, db, v1.PublicID, stamp)
	backdateRecord(t, db, v2.PublicID, stamp)

	latest, err := db.ListLatestReleaseRecords("prod", stamp, 2)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(latest) != 2 || latest[0].Version != "1.1.0" || latest[1].Version != "1.0.0" {
		t.Fatalf("tie order = %v, want later write 1.1.0 then 1.0.0", versionList(latest))
	}
}

func TestEffectiveReleaseRecordAsOf(t *testing.T) {
	db := openTestStore(t)
	v1 := sampleRecord("prod", "1.0.0", "allowed", "1.0.0", entries("a")...)
	if err := db.InsertReleaseRecord(v1); err != nil {
		t.Fatalf("insert: %v", err)
	}
	backdateRecord(t, db, v1.PublicID, "2026-09-01T08:00:00Z")

	inRange, err := db.EffectiveReleaseRecordAsOf("prod", "1.0.0", "2026-09-01T08:00:00Z")
	if err != nil || inRange == nil || inRange.PublicID != v1.PublicID {
		t.Fatalf("in-range resolve = %v, %v", inRange, err)
	}
	outOfRange, err := db.EffectiveReleaseRecordAsOf("prod", "1.0.0", "2026-08-31T23:59:59Z")
	if err != nil || outOfRange != nil {
		t.Fatalf("out-of-range resolve = %v, %v; want nil, nil", outOfRange, err)
	}
	unknownVersion, err := db.EffectiveReleaseRecordAsOf("prod", "9.9.9", "2999-01-01T00:00:00Z")
	if err != nil || unknownVersion != nil {
		t.Fatalf("unknown version = %v, %v; want nil, nil", unknownVersion, err)
	}
}

func versionList(records []ReleaseRecord) []string {
	out := make([]string, 0, len(records))
	for _, record := range records {
		out = append(out, record.Version)
	}
	return out
}
