// Package store owns the SQLite connection and the schema of the deployment records.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"sync"

	_ "modernc.org/sqlite"
)

// Error codes returned to clients. Every response that is not a success carries exactly one of these.
const (
	CodeInvalidRequest      = "invalid_request"
	CodeNotFound            = "not_found"
	CodeConflict            = "conflict"
	CodeStorageUnavailable  = "storage_unavailable"
	CodeInvalidReleaseInput = "invalid_release_input"
	CodeReleaseNotFound     = "release_not_found"
	CodeEnvironmentNotFound = "environment_not_found"
	CodeReleaseConflict     = "release_conflict"
	CodeComparisonConflict  = "comparison_conflict"
)

// Error is the JSON shape of a failed request as described in README.md.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// NewError builds a client-visible error. Messages must stay free of SQL, file paths and stack traces.
func NewError(code, message string) *Error { return &Error{Code: code, Message: message} }

// Is reports whether err carries the given client-visible code.
func Is(err error, code string) bool {
	var target *Error
	return errors.As(err, &target) && target.Code == code
}

// Store is the persistence handle shared by the HTTP layer.
type Store struct {
	db *sql.DB

	// releaseGates serializes concurrent release-record submissions for the
	// same (environment, version) pair. One gate per key, never deleted.
	releaseGates sync.Map
}

// Open connects to the SQLite file and applies the schema.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("store: database path is empty")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db}
	if err := store.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

// Close releases the connection.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Ping reports whether the database answers.
func (s *Store) Ping() error {
	if s == nil || s.db == nil {
		return NewError(CodeStorageUnavailable, "database is not available")
	}
	if err := s.db.Ping(); err != nil {
		return NewError(CodeStorageUnavailable, "database is not available")
	}
	return nil
}

// migrate creates the tables this service owns. It is idempotent.
func (s *Store) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS deployments (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  name        TEXT    NOT NULL,
  environment TEXT    NOT NULL,
  version     TEXT    NOT NULL,
  created_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
`
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	// Columns added after the initial schema. Existing databases gain them one
	// by one; rows written before this change keep NULL and stay readable.
	additions := []struct{ column, statement string }{
		{"changes_json", `ALTER TABLE deployments ADD COLUMN changes_json TEXT`},
		{"gate_status", `ALTER TABLE deployments ADD COLUMN gate_status TEXT`},
		{"rollback_point", `ALTER TABLE deployments ADD COLUMN rollback_point TEXT`},
	}
	for _, addition := range additions {
		exists, err := s.columnExists("deployments", addition.column)
		if err != nil {
			return fmt.Errorf("store: migrate: %w", err)
		}
		if exists {
			continue
		}
		if _, err := s.db.Exec(addition.statement); err != nil {
			return fmt.Errorf("store: migrate: %w", err)
		}
	}
	const trackedSchema = `
CREATE TABLE IF NOT EXISTS tracked_environments (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  environment   TEXT    NOT NULL UNIQUE,
  display_name  TEXT    NOT NULL DEFAULT '',
  registered_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);
CREATE TABLE IF NOT EXISTS release_records (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  public_id      TEXT    NOT NULL UNIQUE,
  environment    TEXT    NOT NULL,
  version        TEXT    NOT NULL,
  batch_id       TEXT    NOT NULL DEFAULT '',
  gate_status    TEXT    NOT NULL,
  rollback_point TEXT    NOT NULL,
  recorded_at    TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);
CREATE TABLE IF NOT EXISTS release_change_entries (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  record_id   INTEGER NOT NULL REFERENCES release_records(id),
  sequence_no INTEGER NOT NULL,
  category    TEXT    NOT NULL,
  title       TEXT    NOT NULL,
  description TEXT    NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS promotion_routes (
  id   INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT    NOT NULL UNIQUE
);
CREATE TABLE IF NOT EXISTS promotion_route_environments (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  route_id    INTEGER NOT NULL REFERENCES promotion_routes(id),
  position    INTEGER NOT NULL,
  environment TEXT    NOT NULL,
  UNIQUE (route_id, position),
  UNIQUE (route_id, environment)
);
CREATE TABLE IF NOT EXISTS release_batch_routes (
  batch_id TEXT    PRIMARY KEY,
  route_id INTEGER NOT NULL REFERENCES promotion_routes(id)
);
CREATE TABLE IF NOT EXISTS release_gate_evaluations (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  record_id   INTEGER NOT NULL UNIQUE REFERENCES release_records(id),
  gate_status TEXT    NOT NULL,
  created_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);
CREATE TABLE IF NOT EXISTS release_gate_checks (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  evaluation_id INTEGER NOT NULL REFERENCES release_gate_evaluations(id),
  position      INTEGER NOT NULL,
  check_name    TEXT    NOT NULL,
  status        TEXT    NOT NULL,
  evidence      TEXT    NOT NULL,
  waiver_reason TEXT    NOT NULL DEFAULT '',
  UNIQUE (evaluation_id, check_name)
);
`
	if _, err := s.db.Exec(trackedSchema); err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	batchColumnExists, err := s.columnExists("release_records", "batch_id")
	if err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	if !batchColumnExists {
		if _, err := s.db.Exec(`ALTER TABLE release_records ADD COLUMN batch_id TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("store: migrate: %w", err)
		}
	}
	// Databases created before release batches kept a table-level
	// UNIQUE (environment, version) constraint. Batched records must stay
	// isolated per batch identifier, so that constraint is replaced by two
	// partial unique indexes: one covering legacy unbatched rows and one
	// scoping (environment, version) duplicates inside a single batch.
	if err := s.migrateReleaseBatchIndexes(); err != nil {
		return err
	}
	return nil
}

// migrateReleaseBatchIndexes swaps the old UNIQUE (environment, version)
// table constraint for batch-aware partial unique indexes. It is idempotent.
func (s *Store) migrateReleaseBatchIndexes() error {
	rows, err := s.db.Query(`PRAGMA index_list('release_records')`)
	if err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	type indexInfo struct {
		name   string
		unique int
		origin string
	}
	var indexes []indexInfo
	for rows.Next() {
		var seq, unique int
		var name, origin, partial sql.NullString
		if err := rows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			rows.Close()
			return fmt.Errorf("store: migrate: %w", err)
		}
		indexes = append(indexes, indexInfo{name: name.String, unique: unique, origin: origin.String})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	needsRebuild := false
	for _, index := range indexes {
		// Origin "u" marks an index created by a table UNIQUE constraint;
		// SQLite cannot drop such an index directly, so the table is rebuilt.
		if index.origin != "u" || index.unique != 1 {
			continue
		}
		columns, err := s.indexColumns(index.name)
		if err != nil {
			return err
		}
		if len(columns) == 2 && columns[0] == "environment" && columns[1] == "version" {
			needsRebuild = true
		}
	}
	if needsRebuild {
		const rebuild = `
CREATE TABLE release_records_new (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  public_id      TEXT    NOT NULL UNIQUE,
  environment    TEXT    NOT NULL,
  version        TEXT    NOT NULL,
  batch_id       TEXT    NOT NULL DEFAULT '',
  gate_status    TEXT    NOT NULL,
  rollback_point TEXT    NOT NULL,
  recorded_at    TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);
INSERT INTO release_records_new
  (id, public_id, environment, version, batch_id, gate_status, rollback_point, recorded_at)
  SELECT id, public_id, environment, version, batch_id, gate_status, rollback_point, recorded_at
  FROM release_records;
DROP TABLE release_records;
ALTER TABLE release_records_new RENAME TO release_records;
`
		if _, err := s.db.Exec(rebuild); err != nil {
			return fmt.Errorf("store: migrate: %w", err)
		}
	}
	const partialIndexes = `
CREATE UNIQUE INDEX IF NOT EXISTS release_records_env_version_unbatched
  ON release_records (environment, version) WHERE batch_id = '';
CREATE UNIQUE INDEX IF NOT EXISTS release_records_env_version_batch
  ON release_records (environment, version, batch_id) WHERE batch_id != '';
`
	if _, err := s.db.Exec(partialIndexes); err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	return nil
}

// indexColumns returns the columns covered by an index in index order.
func (s *Store) indexColumns(indexName string) ([]string, error) {
	rows, err := s.db.Query(`PRAGMA index_info('` + indexName + `')`)
	if err != nil {
		return nil, fmt.Errorf("store: migrate: %w", err)
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var tableOffset, columnOrder int
		var name sql.NullString
		if err := rows.Scan(&tableOffset, &columnOrder, &name); err != nil {
			return nil, fmt.Errorf("store: migrate: %w", err)
		}
		columns = append(columns, name.String)
	}
	return columns, rows.Err()
}

// columnExists reports whether the named column is present on the table.
func (s *Store) columnExists(table, column string) (bool, error) {
	rows, err := s.db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, pk int
		var name, columnType string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}
