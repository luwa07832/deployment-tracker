package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "releases.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func sampleRelease(environment, version, gate, rollback string, changes ...string) Release {
	return Release{
		Environment:   environment,
		Version:       version,
		Changes:       changes,
		GateStatus:    &gate,
		RollbackPoint: &rollback,
	}
}

func TestInsertAndFindReleaseRoundTrip(t *testing.T) {
	db := openTestStore(t)
	rel := sampleRelease("prod", "1.2.0", "allowed", "1.1.0", "add login", "fix logout")
	if err := db.InsertRelease(&rel); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if rel.ID == 0 || rel.CreatedAt == "" {
		t.Fatalf("insert did not fill id/created_at: %+v", rel)
	}
	found, err := db.FindRelease("prod", "1.2.0")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if found == nil {
		t.Fatal("find returned nil for an inserted release")
	}
	if found.ID != rel.ID || found.Environment != "prod" || found.Version != "1.2.0" {
		t.Fatalf("found = %+v, want the inserted record", found)
	}
	if found.GateStatus == nil || *found.GateStatus != "allowed" {
		t.Fatalf("gate_status = %v, want allowed", found.GateStatus)
	}
	if found.RollbackPoint == nil || *found.RollbackPoint != "1.1.0" {
		t.Fatalf("rollback_point = %v, want 1.1.0", found.RollbackPoint)
	}
	if len(found.Changes) != 2 || found.Changes[0] != "add login" || found.Changes[1] != "fix logout" {
		t.Fatalf("changes = %v, want [add login fix logout]", found.Changes)
	}
}

func TestFindReleaseReturnsNilWhenAbsent(t *testing.T) {
	db := openTestStore(t)
	found, err := db.FindRelease("prod", "9.9.9")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if found != nil {
		t.Fatalf("found = %+v, want nil", found)
	}
}

func TestListReleasesFiltersAndOrdering(t *testing.T) {
	db := openTestStore(t)
	seed := []Release{
		sampleRelease("staging", "1.0.0", "allowed", "0.9.0", "add login"),
		sampleRelease("prod", "1.0.0", "blocked", "0.9.0", "add login", "drop table"),
		sampleRelease("prod", "1.1.0", "allowed", "1.0.0", "fix logout"),
	}
	for i := range seed {
		if err := db.InsertRelease(&seed[i]); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	all, err := db.ListReleases(ReleaseFilter{})
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 3 || all[0].Environment != "staging" || all[1].Version != "1.0.0" || all[2].Version != "1.1.0" {
		t.Fatalf("list all = %+v, want the 3 seeded records in registration order", all)
	}

	byEnv, _ := db.ListReleases(ReleaseFilter{Environment: "prod"})
	if len(byEnv) != 2 {
		t.Fatalf("by environment = %d records, want 2", len(byEnv))
	}
	byVersion, _ := db.ListReleases(ReleaseFilter{Version: "1.0.0"})
	if len(byVersion) != 2 {
		t.Fatalf("by version = %d records, want 2", len(byVersion))
	}
	byChange, _ := db.ListReleases(ReleaseFilter{Change: "drop table"})
	if len(byChange) != 1 || byChange[0].Environment != "prod" {
		t.Fatalf("by change = %+v, want the single prod record", byChange)
	}
	byGate, _ := db.ListReleases(ReleaseFilter{GateStatus: "blocked"})
	if len(byGate) != 1 || byGate[0].Version != "1.0.0" {
		t.Fatalf("by gate_status = %+v, want the single blocked record", byGate)
	}
	byRollback, _ := db.ListReleases(ReleaseFilter{RollbackPoint: "1.0.0"})
	if len(byRollback) != 1 || byRollback[0].Version != "1.1.0" {
		t.Fatalf("by rollback_point = %+v, want the 1.1.0 record", byRollback)
	}
	combined, _ := db.ListReleases(ReleaseFilter{Environment: "prod", GateStatus: "allowed"})
	if len(combined) != 1 || combined[0].Version != "1.1.0" {
		t.Fatalf("combined filter = %+v, want the prod 1.1.0 record", combined)
	}

	empty, err := db.ListReleases(ReleaseFilter{Version: "no-such-version"})
	if err != nil {
		t.Fatalf("list empty: %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Fatalf("empty result = %#v, want a non-nil empty slice", empty)
	}
}

func TestEnvironmentExists(t *testing.T) {
	db := openTestStore(t)
	exists, err := db.EnvironmentExists("prod")
	if err != nil || exists {
		t.Fatalf("exists before insert = %v, %v; want false, nil", exists, err)
	}
	rel := sampleRelease("prod", "1.0.0", "pending", "0.9.0", "init")
	if err := db.InsertRelease(&rel); err != nil {
		t.Fatalf("insert: %v", err)
	}
	exists, err = db.EnvironmentExists("prod")
	if err != nil || !exists {
		t.Fatalf("exists after insert = %v, %v; want true, nil", exists, err)
	}
}

// TestLegacyDatabaseStaysReadable simulates a database written before change
// tracking existed: the migration must add the new columns without touching
// old rows, and old rows must read back with the new fields absent.
func TestLegacyDatabaseStaysReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	if _, err := raw.Exec(`CREATE TABLE deployments (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL,
		environment TEXT NOT NULL,
		version TEXT NOT NULL,
		created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
	)`); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO deployments (name, environment, version) VALUES ('legacy', 'prod', '1.0.0')`); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO deployments (name, environment, version) VALUES ('legacy2', 'prod', '1.0.0')`); err != nil {
		t.Fatalf("insert second legacy row: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("raw close: %v", err)
	}

	db, err := Open(path)
	if err != nil {
		t.Fatalf("reopen with migration: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	for _, column := range []string{"changes_json", "gate_status", "rollback_point"} {
		exists, err := db.columnExists("deployments", column)
		if err != nil || !exists {
			t.Fatalf("column %s exists = %v, %v; want true, nil", column, exists, err)
		}
	}

	// The legacy duplicate resolves to the record registered last.
	found, err := db.FindRelease("prod", "1.0.0")
	if err != nil {
		t.Fatalf("find legacy: %v", err)
	}
	if found == nil || found.Name != "legacy2" {
		t.Fatalf("find legacy = %+v, want the later legacy2 row", found)
	}
	if found.Changes != nil || found.GateStatus != nil || found.RollbackPoint != nil {
		t.Fatalf("legacy record gained new fields: %+v", found)
	}
	history, err := db.ListReleases(ReleaseFilter{Environment: "prod"})
	if err != nil {
		t.Fatalf("list legacy: %v", err)
	}
	if len(history) != 2 || history[0].Name != "legacy" || history[1].Name != "legacy2" {
		t.Fatalf("legacy history = %+v, want both rows in registration order", history)
	}
}

func TestListReleaseHistoryKeysetPaging(t *testing.T) {
	db := openTestStore(t)
	// Five rows; the middle three share one timestamp so registration id is
	// the tie breaker and must keep paging stable.
	ordered := []struct {
		version  string
		insertAt string
	}{
		{"1.0.0", "2026-10-01T08:00:00Z"},
		{"1.0.1", "2026-10-01T09:00:00Z"},
		{"1.0.2", "2026-10-01T09:00:00Z"},
		{"1.0.3", "2026-10-01T09:00:00Z"},
		{"1.0.4", "2026-10-01T10:00:00Z"},
	}
	for _, item := range ordered {
		gate, rollback := "allowed", "0.9.0"
		rel := Release{
			Environment: "prod", Version: item.version, Changes: []string{"c"},
			GateStatus: &gate, RollbackPoint: &rollback,
		}
		if err := db.InsertRelease(&rel); err != nil {
			t.Fatalf("insert %s: %v", item.version, err)
		}
		if _, err := db.db.Exec(
			`UPDATE deployments SET created_at = ? WHERE id = ?`, item.insertAt, rel.ID,
		); err != nil {
			t.Fatalf("pin created_at for %s: %v", item.version, err)
		}
	}
	var (
		afterID   int64
		afterAt   string
		collected []string
		pages     int
	)
	for {
		page, err := db.ListReleaseHistory("prod", 2, afterID, afterAt)
		if err != nil {
			t.Fatalf("list history page: %v", err)
		}
		for i := range page.Releases {
			collected = append(collected, page.Releases[i].Version)
		}
		pages++
		if !page.HasNext {
			break
		}
		afterID, afterAt = page.NextLastID, page.NextLastAt
	}
	want := []string{"1.0.4", "1.0.3", "1.0.2", "1.0.1", "1.0.0"}
	if len(collected) != len(want) {
		t.Fatalf("collected %v, want %v", collected, want)
	}
	for i := range want {
		if collected[i] != want[i] {
			t.Fatalf("collected %v, want %v (newest first, id tie-break)", collected, want)
		}
	}
	if pages != 3 {
		t.Fatalf("pages = %d, want 3 (2+2+1)", pages)
	}
}

func TestListReleaseHistoryScopesByEnvironment(t *testing.T) {
	db := openTestStore(t)
	for _, item := range []struct{ env, version string }{
		{"prod", "1.0.0"}, {"stage", "1.0.0"}, {"prod", "1.0.1"},
	} {
		rel := sampleRelease(item.env, item.version, "allowed", "0.9.0", "c")
		if err := db.InsertRelease(&rel); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	page, err := db.ListReleaseHistory("prod", 50, 0, "")
	if err != nil {
		t.Fatalf("list history: %v", err)
	}
	if len(page.Releases) != 2 || page.HasNext {
		t.Fatalf("page = %+v, want both prod rows and no continuation", page)
	}
}
