package store

import (
	"path/filepath"
	"testing"
)

func TestOpenCreatesTheSchemaAndAnswersPing(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	var count int
	if err := db.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'deployments'`).Scan(&count); err != nil {
		t.Fatalf("inspect schema: %v", err)
	}
	if count != 1 {
		t.Fatalf("deployments table count = %d, want 1", count)
	}
}

func TestClosedStoreReportsStorageUnavailable(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := db.Ping(); !Is(err, CodeStorageUnavailable) {
		t.Fatalf("ping after close = %v, want %s", err, CodeStorageUnavailable)
	}
}
