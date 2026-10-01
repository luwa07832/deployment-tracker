package store

import (
	"errors"
	"path/filepath"
	"testing"
)

func sampleRecord(environment, version, gate, rollback string, changes ...ChangeEntry) *ReleaseRecord {
	return &ReleaseRecord{
		Environment:   environment,
		Version:       version,
		GateStatus:    gate,
		RollbackPoint: rollback,
		Changes:       changes,
	}
}

func entries(titles ...string) []ChangeEntry {
	out := make([]ChangeEntry, 0, len(titles))
	for i, title := range titles {
		out = append(out, ChangeEntry{Sequence: i + 1, Category: "change", Title: title, Description: "desc " + title})
	}
	return out
}

func TestEnsureEnvironmentIdempotentAndList(t *testing.T) {
	db := openTestStore(t)
	first := &TrackedEnvironment{Environment: "prod", DisplayName: "Production"}
	created, err := db.EnsureEnvironment(first)
	if err != nil || !created || first.RegisteredAt == "" {
		t.Fatalf("first ensure = %v, %v; want created with timestamp", first, err)
	}
	again := &TrackedEnvironment{Environment: "prod", DisplayName: "Ignored"}
	created, err = db.EnsureEnvironment(again)
	if err != nil || created {
		t.Fatalf("second ensure = %v, %v; want idempotent no-op", again, err)
	}
	if again.DisplayName != "Production" || again.RegisteredAt != first.RegisteredAt {
		t.Fatalf("idempotent ensure changed the row: %+v", again)
	}
	exists, err := db.TrackedEnvironmentExists("prod")
	if err != nil || !exists {
		t.Fatalf("exists = %v, %v; want true", exists, err)
	}
	got, err := db.GetEnvironment("missing")
	if err != nil || got != nil {
		t.Fatalf("missing env = %v, %v; want nil, nil", got, err)
	}
	if _, err := db.EnsureEnvironment(&TrackedEnvironment{Environment: "staging"}); err != nil {
		t.Fatalf("ensure staging: %v", err)
	}
	list, err := db.ListEnvironments()
	if err != nil || len(list) != 2 || list[0].Environment != "prod" || list[1].Environment != "staging" {
		t.Fatalf("list = %+v, %v; want prod then staging", list, err)
	}
}

func TestInsertGetReleaseRecordRoundTrip(t *testing.T) {
	db := openTestStore(t)
	if _, err := db.EnsureEnvironment(&TrackedEnvironment{Environment: "prod"}); err != nil {
		t.Fatalf("ensure env: %v", err)
	}
	record := sampleRecord("prod", "1.2.0", "allowed", "snapshot:1.1.0",
		ChangeEntry{Sequence: 1, Category: "feature", Title: "add login", Description: "users can log in"},
		ChangeEntry{Sequence: 2, Category: "fix", Title: "fix logout", Description: "logout clears the session"},
	)
	if err := db.InsertReleaseRecord(record); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if record.PublicID == "" || len(record.PublicID) != len("rel_")+32 {
		t.Fatalf("public id = %q, want rel_ plus 32 hex chars", record.PublicID)
	}
	if record.RecordedAt == "" {
		t.Fatal("recorded_at was not filled by the server")
	}
	if record.Changes[0].Title != "add login" {
		t.Fatalf("changes round trip = %+v", record.Changes)
	}
	got, err := db.GetReleaseRecord(record.PublicID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil || got.PublicID != record.PublicID {
		t.Fatalf("got = %+v, want the inserted record", got)
	}
	if got.RollbackPoint != "snapshot:1.1.0" || got.GateStatus != "allowed" {
		t.Fatalf("got = %+v", got)
	}
	if len(got.Changes) != 2 || got.Changes[1].Sequence != 2 || got.Changes[1].Category != "fix" ||
		got.Changes[1].Description != "logout clears the session" {
		t.Fatalf("change entries = %+v", got.Changes)
	}
}

func TestInsertDuplicateEnvironmentVersion(t *testing.T) {
	db := openTestStore(t)
	if _, err := db.EnsureEnvironment(&TrackedEnvironment{Environment: "prod"}); err != nil {
		t.Fatalf("ensure env: %v", err)
	}
	first := sampleRecord("prod", "1.0.0", "allowed", "0.9.0", entries("a")...)
	if err := db.InsertReleaseRecord(first); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	duplicate := sampleRecord("prod", "1.0.0", "blocked", "0.8.0", entries("b")...)
	err := db.InsertReleaseRecord(duplicate)
	var conflict *ErrReleaseAlreadyExists
	if !errors.As(err, &conflict) || conflict.Environment != "prod" || conflict.Version != "1.0.0" {
		t.Fatalf("duplicate err = %v, want *ErrReleaseAlreadyExists prod 1.0.0", err)
	}
	// The same version is allowed for a different environment.
	if _, err := db.EnsureEnvironment(&TrackedEnvironment{Environment: "staging"}); err != nil {
		t.Fatalf("ensure env: %v", err)
	}
	if err := db.InsertReleaseRecord(sampleRecord("staging", "1.0.0", "allowed", "0.9.0", entries("a")...)); err != nil {
		t.Fatalf("cross-environment insert: %v", err)
	}
}

func TestListReleaseRecordsFilteringAndNewestFirst(t *testing.T) {
	db := openTestStore(t)
	for _, env := range []string{"prod", "staging"} {
		if _, err := db.EnsureEnvironment(&TrackedEnvironment{Environment: env}); err != nil {
			t.Fatalf("ensure %s: %v", env, err)
		}
	}
	seed := []*ReleaseRecord{
		sampleRecord("prod", "1.0.0", "blocked", "0.9.0", entries("old")...),
		sampleRecord("staging", "1.0.0", "allowed", "0.9.0", entries("mid")...),
		sampleRecord("prod", "1.2.0", "allowed", "1.1.0", entries("new")...),
	}
	for _, record := range seed {
		if err := db.InsertReleaseRecord(record); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	all, err := db.ListReleaseRecords(ReleaseRecordFilter{})
	if err != nil || len(all) != 3 {
		t.Fatalf("list all = %d records, %v", len(all), err)
	}
	if all[0].Version != "1.2.0" || all[1].Environment != "staging" || all[2].Version != "1.0.0" {
		t.Fatalf("newest-first order = %+v %+v %+v", all[0], all[1], all[2])
	}
	prod, _ := db.ListReleaseRecords(ReleaseRecordFilter{Environment: "prod"})
	if len(prod) != 2 || prod[0].Version != "1.2.0" {
		t.Fatalf("env filter = %+v", prod)
	}
	allowed, _ := db.ListReleaseRecords(ReleaseRecordFilter{GateStatus: "allowed"})
	if len(allowed) != 2 {
		t.Fatalf("gate filter = %d, want 2", len(allowed))
	}
	one, _ := db.ListReleaseRecords(ReleaseRecordFilter{Environment: "prod", Version: "1.0.0"})
	if len(one) != 1 || one[0].Version != "1.0.0" {
		t.Fatalf("combined filter = %+v", one)
	}
	empty, err := db.ListReleaseRecords(ReleaseRecordFilter{Version: "9.9.9"})
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("empty = %#v, %v; want non-nil empty slice", empty, err)
	}
}

func TestListReleaseRecordsTimeBoundsAreInclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bounds.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.EnsureEnvironment(&TrackedEnvironment{Environment: "prod"}); err != nil {
		t.Fatalf("ensure env: %v", err)
	}
	record := sampleRecord("prod", "1.0.0", "allowed", "0.9.0")
	if err := db.InsertReleaseRecord(record); err != nil {
		t.Fatalf("insert: %v", err)
	}
	at := record.RecordedAt
	if at == "" {
		t.Fatal("recorded_at empty")
	}
	cases := []struct {
		name   string
		filter ReleaseRecordFilter
		want   int
	}{
		{"same instant from", ReleaseRecordFilter{From: at}, 1},
		{"same instant to", ReleaseRecordFilter{To: at}, 1},
		{"later from", ReleaseRecordFilter{From: "9999-01-01T00:00:00Z"}, 0},
		{"earlier to", ReleaseRecordFilter{To: "2000-01-01T00:00:00Z"}, 0},
	}
	for _, tc := range cases {
		got, err := db.ListReleaseRecords(tc.filter)
		if err != nil || len(got) != tc.want {
			t.Fatalf("%s = %d records, %v; want %d", tc.name, len(got), err, tc.want)
		}
	}
}

func TestEffectiveReleaseRecordReadsBack(t *testing.T) {
	db := openTestStore(t)
	if _, err := db.EnsureEnvironment(&TrackedEnvironment{Environment: "prod"}); err != nil {
		t.Fatalf("ensure env: %v", err)
	}
	record := sampleRecord("prod", "1.0.0", "pending", "0.9.0", entries("a")...)
	if err := db.InsertReleaseRecord(record); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, err := db.EffectiveReleaseRecord("prod", "1.0.0")
	if err != nil || got == nil || got.PublicID != record.PublicID {
		t.Fatalf("effective = %+v, %v", got, err)
	}
	missing, err := db.EffectiveReleaseRecord("prod", "9.9.9")
	if err != nil || missing != nil {
		t.Fatalf("missing = %+v, %v", missing, err)
	}
}
