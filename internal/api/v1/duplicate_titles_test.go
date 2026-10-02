package v1

import (
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/luwa07832/deployment-tracker/internal/store"
)

// newTestRouterWithDeps builds a router and also returns its store so tests
// can seed legacy data that the current write API would reject, such as
// release records with repeated change-entry titles.
func newTestRouterWithDeps(t *testing.T) (*gin.Engine, Dependencies) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := store.Open(filepath.Join(t.TempDir(), "v1-legacy.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	deps := Dependencies{Store: db}
	engine := gin.New()
	Register(engine, deps)
	return engine, deps
}

// legacyDuplicateRecord inserts a release record whose change entries reuse a
// title. The write API rejects such payloads now, but already-saved records
// keep every entry and the read/diff paths must handle them item by item.
func legacyDuplicateRecord(t *testing.T, deps Dependencies, environment, version, gate, rollback string,
	changes ...store.ChangeEntry) *store.ReleaseRecord {
	t.Helper()
	record := &store.ReleaseRecord{
		Environment:   environment,
		Version:       version,
		GateStatus:    gate,
		RollbackPoint: rollback,
		Changes:       changes,
	}
	if err := deps.Store.InsertReleaseRecord(record); err != nil {
		t.Fatalf("insert legacy record %s %s: %v", environment, version, err)
	}
	return record
}

func legacyEnv(t *testing.T, deps Dependencies, key string) {
	t.Helper()
	if _, err := deps.Store.EnsureEnvironment(&store.TrackedEnvironment{Environment: key}); err != nil {
		t.Fatalf("register environment %s: %v", key, err)
	}
}

func dupEntry(seq int, category, title, description string) store.ChangeEntry {
	return store.ChangeEntry{Sequence: seq, Category: category, Title: title, Description: description}
}
