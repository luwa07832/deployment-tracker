package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func TestBatchRecordsIsolatedAndUnbatchedUnique(t *testing.T) {
	db := openTestStore(t)
	if _, err := db.EnsureEnvironment(&TrackedEnvironment{Environment: "prod"}); err != nil {
		t.Fatalf("ensure env: %v", err)
	}

	unbatched := &ReleaseRecord{
		Environment: "prod", Version: "1.0.0", GateStatus: "allowed",
		RollbackPoint: "snap:1", RecordedAt: "2026-08-31T10:00:00Z", Changes: []ChangeEntry{},
	}
	if err := db.InsertReleaseRecord(unbatched); err != nil {
		t.Fatalf("insert unbatched: %v", err)
	}
	if err := db.InsertReleaseRecord(&ReleaseRecord{
		Environment: "prod", Version: "1.0.0", GateStatus: "allowed",
		RollbackPoint: "snap:1", Changes: []ChangeEntry{},
	}); err == nil {
		t.Fatal("duplicate unbatched record must fail")
	} else {
		var duplicate *ErrReleaseAlreadyExists
		if !errors.As(err, &duplicate) {
			t.Fatalf("want ErrReleaseAlreadyExists, got %v", err)
		}
	}

	for _, batch := range []string{"x", "y"} {
		err := db.InsertReleaseRecord(&ReleaseRecord{
			Environment: "prod", Version: "1.0.0", GateStatus: "allowed",
			RollbackPoint: "snap:" + batch, BatchID: batch,
			RecordedAt: "2026-08-30T10:00:00Z", Changes: []ChangeEntry{},
		})
		if err != nil {
			t.Fatalf("batch %s must coexist with unbatched and other batches: %v", batch, err)
		}
	}

	// Repeating the same (batch, env, version) still conflicts.
	err := db.InsertReleaseRecord(&ReleaseRecord{
		Environment: "prod", Version: "1.0.0", GateStatus: "allowed",
		RollbackPoint: "snap:x2", BatchID: "x", Changes: []ChangeEntry{},
	})
	var duplicate *ErrReleaseAlreadyExists
	if !errors.As(err, &duplicate) {
		t.Fatalf("same batch/env/version must conflict, got %v", err)
	}

	// A batch can record multiple releases into one environment, chronologically.
	for i, version := range []string{"1.1.0", "1.2.0"} {
		record := &ReleaseRecord{
			Environment: "prod", Version: version, GateStatus: "allowed",
			RollbackPoint: "snap", BatchID: "x",
			RecordedAt: "2026-09-0" + string(rune('1'+i)) + "T10:00:00Z",
			Changes:    []ChangeEntry{},
		}
		if err := db.InsertReleaseRecord(record); err != nil {
			t.Fatalf("second release in batch: %v", err)
		}
	}
	facts, err := db.ListBatchReleaseRecords("x")
	if err != nil {
		t.Fatalf("list batch: %v", err)
	}
	if len(facts) != 3 {
		t.Fatalf("batch x facts = %d, want 3", len(facts))
	}
	if facts[0].Version != "1.0.0" || facts[2].Version != "1.2.0" {
		t.Fatalf("batch facts not chronological: %v %v", facts[0].Version, facts[2].Version)
	}

	exists, err := db.BatchExists("y")
	if err != nil || !exists {
		t.Fatalf("BatchExists y = %v, %v", exists, err)
	}
	exists, err = db.BatchExists("zzz")
	if err != nil || exists {
		t.Fatalf("BatchExists zzz = %v, %v", exists, err)
	}
}

func TestMigrationPreservesLegacyRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	_, err = raw.Exec(`
CREATE TABLE tracked_environments (
  id INTEGER PRIMARY KEY AUTOINCREMENT, environment TEXT NOT NULL UNIQUE,
  display_name TEXT NOT NULL DEFAULT '',
  registered_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')));
CREATE TABLE release_records (
  id INTEGER PRIMARY KEY AUTOINCREMENT, public_id TEXT NOT NULL UNIQUE,
  environment TEXT NOT NULL, version TEXT NOT NULL, gate_status TEXT NOT NULL,
  rollback_point TEXT NOT NULL,
  recorded_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
  UNIQUE (environment, version));
CREATE TABLE release_change_entries (
  id INTEGER PRIMARY KEY AUTOINCREMENT, record_id INTEGER NOT NULL REFERENCES release_records(id),
  sequence_no INTEGER NOT NULL, category TEXT NOT NULL, title TEXT NOT NULL, description TEXT NOT NULL DEFAULT '');
INSERT INTO release_records (public_id, environment, version, gate_status, rollback_point, recorded_at)
  VALUES ('rel_legacy1', 'prod', '9.9.9', 'allowed', 'snap:old', '2026-01-01T00:00:00Z');
INSERT INTO release_change_entries (record_id, sequence_no, category, title, description)
  VALUES (1, 1, 'feature', 'legacy', 'legacy desc');`)
	if err != nil {
		t.Fatalf("seed legacy schema: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	db, err := Open(path)
	if err != nil {
		t.Fatalf("migrate legacy: %v", err)
	}
	defer db.Close()

	record, err := db.GetReleaseRecord("rel_legacy1")
	if err != nil {
		t.Fatalf("read legacy: %v", err)
	}
	if record == nil || record.Version != "9.9.9" || len(record.Changes) != 1 ||
		record.Changes[0].Title != "legacy" {
		t.Fatalf("legacy record not preserved: %+v", record)
	}
	if record.BatchID != "" {
		t.Fatalf("legacy record must keep empty batch, got %q", record.BatchID)
	}
}
