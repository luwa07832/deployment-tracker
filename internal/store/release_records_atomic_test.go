package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	_ "modernc.org/sqlite"
)

// TestInsertReleaseRecordRollsBackAndAllowsRetry forces a failure while the
// change entries are being saved. The main row must be rolled back so the
// identical retry succeeds instead of colliding with a half-written record.
func TestInsertReleaseRecordRollsBackAndAllowsRetry(t *testing.T) {
	db := openTestStore(t)
	if _, err := db.EnsureEnvironment(&TrackedEnvironment{Environment: "prod"}); err != nil {
		t.Fatalf("ensure env: %v", err)
	}
	record := sampleRecord("prod", "1.0.0", "allowed", "rb:0.9.0", entries("a", "b")...)

	hookFired := false
	restore := SetReleaseInsertFaultHookForTest(func(r *ReleaseRecord, attempt int, rowID int64) error {
		if !hookFired {
			hookFired = true
			return errors.New("disk on fire")
		}
		return nil
	})
	defer restore()

	if err := db.InsertReleaseRecord(record); err == nil {
		t.Fatal("first insert must fail when a change entry write fails")
	}
	listed, err := db.ListReleaseRecords(ReleaseRecordFilter{Environment: "prod", Version: "1.0.0"})
	if err != nil {
		t.Fatalf("list after failure: %v", err)
	}
	if len(listed) != 0 {
		t.Fatalf("rolled-back insert left %d queryable records: %+v", len(listed), listed)
	}
	var entryCount int
	if err := db.db.QueryRow(`SELECT count(*) FROM release_change_entries`).Scan(&entryCount); err != nil {
		t.Fatalf("count change entries: %v", err)
	}
	if entryCount != 0 {
		t.Fatalf("rolled-back insert left %d change entries", entryCount)
	}

	retry := sampleRecord("prod", "1.0.0", "allowed", "rb:0.9.0", entries("a", "b")...)
	if err := db.InsertReleaseRecord(retry); err != nil {
		t.Fatalf("retry after rollback must create the record: %v", err)
	}
	if retry.PublicID == "" || len(retry.Changes) != 2 {
		t.Fatalf("retried record incomplete: %+v", retry)
	}
}

// TestConcurrentReleaseSubmissionsOnlyOneWins fires several valid submissions
// for the same environment and version at the same time, including different
// batch ids and payloads. Exactly one must be stored and visible.
func TestConcurrentReleaseSubmissionsOnlyOneWins(t *testing.T) {
	db := openTestStore(t)
	if _, err := db.EnsureEnvironment(&TrackedEnvironment{Environment: "prod"}); err != nil {
		t.Fatalf("ensure env: %v", err)
	}

	const total = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, total)
	records := make([]*ReleaseRecord, total)
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			record := sampleRecord("prod", "1.0.0", "allowed", "rb:0.9.0",
				ChangeEntry{Sequence: 1, Category: "feature", Title: "change", Description: "desc"})
			record.GateStatus = []string{"allowed", "blocked", "pending"}[i%3]
			if i%2 == 1 {
				record.BatchID = "batch-" + string(rune('a'+i))
			}
			records[i] = record
			<-start
			errs[i] = db.InsertReleaseRecord(record)
		}(i)
	}
	close(start)
	wg.Wait()

	winners, conflicts := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			winners++
		case errors.As(err, new(*ErrReleaseAlreadyExists)):
			conflicts++
		default:
			t.Fatalf("unexpected insert error: %v", err)
		}
	}
	if winners != 1 || conflicts != total-1 {
		t.Fatalf("winners = %d conflicts = %d, want exactly 1 winner and %d conflicts", winners, conflicts, total-1)
	}

	listed, err := db.ListReleaseRecords(ReleaseRecordFilter{Environment: "prod", Version: "1.0.0"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("parallel effective versions visible = %d: %+v", len(listed), listed)
	}
	if len(listed[0].Changes) != 1 {
		t.Fatalf("winner must carry its full changes: %+v", listed[0])
	}

	// A later, genuinely sequential submission for a distinct batch still
	// follows the batch-isolation model and conflicts with the stored row
	// only according to the baseline rules.
	effective, err := db.EffectiveReleaseRecord("prod", "1.0.0")
	if err != nil || effective == nil || effective.PublicID != listed[0].PublicID {
		t.Fatalf("effective record = %+v, %v", effective, err)
	}
}

// TestSequentialDistinctBatchesStillAllowed guards the unchanged registration
// model: only submissions racing in one wave are globally exclusive.
func TestSequentialDistinctBatchesStillAllowed(t *testing.T) {
	db := openTestStore(t)
	if _, err := db.EnsureEnvironment(&TrackedEnvironment{Environment: "prod"}); err != nil {
		t.Fatalf("ensure env: %v", err)
	}
	first := sampleRecord("prod", "2.0.0", "allowed", "rb:1.9.0", entries("a")...)
	first.BatchID = "batch-1"
	if err := db.InsertReleaseRecord(first); err != nil {
		t.Fatalf("first batch insert: %v", err)
	}
	second := sampleRecord("prod", "2.0.0", "blocked", "rb:1.8.0", entries("b")...)
	second.BatchID = "batch-2"
	if err := db.InsertReleaseRecord(second); err != nil {
		t.Fatalf("sequential distinct batch insert must stay allowed: %v", err)
	}
}

// TestFirstWriteAfterLegacyDatabaseUpgrade opens a database with the oldest
// release_records schema and performs one atomic insert, making sure the
// upgraded database serves a full record with change entries.
func TestFirstWriteAfterLegacyDatabaseUpgrade(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	_, err = raw.Exec(`
CREATE TABLE tracked_environments (
  id INTEGER PRIMARY KEY AUTOINCREMENT, environment TEXT NOT NULL UNIQUE,
  display_name TEXT NOT NULL DEFAULT '',
  registered_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);
INSERT INTO tracked_environments (environment) VALUES ('prod');
CREATE TABLE release_records (
  id INTEGER PRIMARY KEY AUTOINCREMENT, public_id TEXT NOT NULL UNIQUE,
  environment TEXT NOT NULL, version TEXT NOT NULL,
  gate_status TEXT NOT NULL, rollback_point TEXT NOT NULL,
  recorded_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
  UNIQUE (environment, version)
);
CREATE TABLE release_change_entries (
  id INTEGER PRIMARY KEY AUTOINCREMENT, record_id INTEGER NOT NULL REFERENCES release_records(id),
  sequence_no INTEGER NOT NULL, category TEXT NOT NULL, title TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT ''
);
`)
	if err != nil {
		t.Fatalf("seed legacy schema: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	db, err := Open(path)
	if err != nil {
		t.Fatalf("open migrated db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	record := sampleRecord("prod", "9.9.9", "allowed", "rb:9.8.0",
		ChangeEntry{Sequence: 1, Category: "feature", Title: "first after upgrade", Description: "atomic"},
		ChangeEntry{Sequence: 2, Category: "fix", Title: "second after upgrade", Description: "also atomic"},
	)
	if err := db.InsertReleaseRecord(record); err != nil {
		t.Fatalf("first write after upgrade: %v", err)
	}
	stored, err := db.GetReleaseRecord(record.PublicID)
	if err != nil {
		t.Fatalf("read upgraded write: %v", err)
	}
	if stored == nil || len(stored.Changes) != 2 ||
		stored.Changes[0].Title != "first after upgrade" || stored.Changes[1].Title != "second after upgrade" {
		t.Fatalf("upgraded write incomplete: %+v", stored)
	}
}
