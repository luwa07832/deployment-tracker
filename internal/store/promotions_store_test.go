package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestBatchIsolationAcrossBatchesSharingVersion(t *testing.T) {
	db := openTestStore(t)
	for _, env := range []string{"dev", "test", "prod"} {
		if _, err := db.EnsureEnvironment(&TrackedEnvironment{Environment: env}); err != nil {
			t.Fatalf("ensure %s: %v", env, err)
		}
	}
	batchA := sampleRecord("dev", "1.0.0", "allowed", "rb:0.9.0", entries("a")...)
	batchA.BatchID = "batch-1"
	batchB := sampleRecord("dev", "1.0.0", "allowed", "rb:0.9.0", entries("b")...)
	batchB.BatchID = "batch-2"
	if err := db.InsertReleaseRecord(batchA); err != nil {
		t.Fatalf("insert batch-1: %v", err)
	}
	if err := db.InsertReleaseRecord(batchB); err != nil {
		t.Fatalf("same version in a different batch must be allowed: %v", err)
	}
	// Re-posting the same batch/env/version is still a conflict.
	dup := sampleRecord("dev", "1.0.0", "blocked", "rb:0.8.0", entries("c")...)
	dup.BatchID = "batch-1"
	err := db.InsertReleaseRecord(dup)
	var conflict *ErrReleaseAlreadyExists
	if !errors.As(err, &conflict) {
		t.Fatalf("same-batch duplicate err = %v, want *ErrReleaseAlreadyExists", err)
	}
	gotA, err := db.ListReleaseRecords(ReleaseRecordFilter{BatchID: "batch-1"})
	if err != nil || len(gotA) != 1 || gotA[0].BatchID != "batch-1" || len(gotA[0].Changes) != 1 {
		t.Fatalf("batch-1 filter = %+v, %v", gotA, err)
	}
	exists, err := db.ReleaseBatchExists("batch-2")
	if err != nil || !exists {
		t.Fatalf("batch-2 exists = %v, %v", exists, err)
	}
	missing, err := db.ReleaseBatchExists("batch-nope")
	if err != nil || missing {
		t.Fatalf("missing batch exists = %v, %v", missing, err)
	}
}

func TestUnbatchedAndBatchedRecordsStayDistinct(t *testing.T) {
	db := openTestStore(t)
	if _, err := db.EnsureEnvironment(&TrackedEnvironment{Environment: "prod"}); err != nil {
		t.Fatalf("ensure env: %v", err)
	}
	legacy := sampleRecord("prod", "1.0.0", "allowed", "rb:0.9.0", entries("a")...)
	if err := db.InsertReleaseRecord(legacy); err != nil {
		t.Fatalf("insert unbatched: %v", err)
	}
	batched := sampleRecord("prod", "1.0.0", "allowed", "rb:0.9.0", entries("a")...)
	batched.BatchID = "b1"
	if err := db.InsertReleaseRecord(batched); err == nil {
		t.Fatal("a batched record for an already-used unbatched env/version must conflict")
	}
}

func TestMigratesLegacyUniqueConstraintToBatchIndexes(t *testing.T) {
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
INSERT INTO release_records (id, public_id, environment, version, gate_status, rollback_point)
  VALUES (7, 'rel_legacy0000000000000000000000000001', 'prod', '1.0.0', 'allowed', 'rb:0.9.0');
INSERT INTO release_change_entries (record_id, sequence_no, category, title, description)
  VALUES (7, 1, 'feature', 'legacy change', 'kept through rebuild');
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

	got, err := db.GetReleaseRecord("rel_legacy0000000000000000000000000001")
	if err != nil || got == nil || got.BatchID != "" {
		t.Fatalf("legacy row = %+v, %v", got, err)
	}
	if len(got.Changes) != 1 || got.Changes[0].Title != "legacy change" {
		t.Fatalf("legacy change entries must survive the rebuild: %+v", got.Changes)
	}
	if _, err := db.EnsureEnvironment(&TrackedEnvironment{Environment: "prod"}); err != nil {
		t.Fatalf("ensure prod: %v", err)
	}
	batched := sampleRecord("prod", "2.0.0", "allowed", "rb:1.9.0", entries("a")...)
	batched.BatchID = "batch-x"
	if err := db.InsertReleaseRecord(batched); err != nil {
		t.Fatalf("batched insert after migration: %v", err)
	}
	// Reopening must be a no-op migration (indexes already present).
	if err := db.migrateReleaseBatchIndexes(); err != nil {
		t.Fatalf("idempotent migration: %v", err)
	}
}

func TestBatchNodeFactsAreChronologicalAcrossSameSecondWrites(t *testing.T) {
	db := openTestStore(t)
	for _, env := range []string{"dev", "test"} {
		if _, err := db.EnsureEnvironment(&TrackedEnvironment{Environment: env}); err != nil {
			t.Fatalf("ensure %s: %v", env, err)
		}
	}
	// Insert several batched facts for dev sharing one recorded_at second.
	// ListReleaseRecords is newest-first; reversing must recover write order.
	insert := func(version string) {
		t.Helper()
		record := sampleRecord("dev", version, "allowed", "rb:0.9.0", entries("a:"+version)...)
		record.BatchID = "batch-time"
		if err := db.InsertReleaseRecord(record); err != nil {
			t.Fatalf("insert %s: %v", version, err)
		}
	}
	insert("1.0.0")
	insert("1.0.1")
	insert("1.0.2")
	listed, err := db.ListReleaseRecords(ReleaseRecordFilter{BatchID: "batch-time", Environment: "dev"})
	if err != nil || len(listed) != 3 {
		t.Fatalf("list = %+v, %v", listed, err)
	}
	if listed[0].Version != "1.0.2" || listed[2].Version != "1.0.0" {
		t.Fatalf("newest-first order = %s, %s, %s", listed[0].Version, listed[1].Version, listed[2].Version)
	}
	// Reversing yields chronological write order, the order chain nodes use.
	if listed[len(listed)-1].Version != "1.0.0" || listed[0].Version != "1.0.2" {
		t.Fatalf("reversed order must be 1.0.0..1.0.2")
	}
	exists, err := db.ReleaseBatchExists("batch-time")
	if err != nil || !exists {
		t.Fatalf("exists = %v, %v", exists, err)
	}
}
