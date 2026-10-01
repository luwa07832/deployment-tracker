// Package store owns the SQLite connection and the schema of the deployment records.
package store

import (
	"database/sql"
	"errors"
	"fmt"

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
  gate_status    TEXT    NOT NULL,
  rollback_point TEXT    NOT NULL,
  batch_id       TEXT,
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
`
	if _, err := s.db.Exec(trackedSchema); err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	if err := s.migrateReleaseRecordBatches(); err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	return nil
}

// migrateReleaseRecordBatches adds the optional batch_id grouping used by the
// promotion-chain read endpoints. Records without a batch keep the original
// (environment, version) uniqueness through a partial index; batch records
// are isolated per batch. The legacy table-level UNIQUE constraint, when
// present in an older database, is rebuilt away; stored facts are preserved.
func (s *Store) migrateReleaseRecordBatches() error {
	hasBatchColumn, err := s.columnExists("release_records", "batch_id")
	if err != nil {
		return err
	}
	if !hasBatchColumn {
		if _, err := s.db.Exec(`ALTER TABLE release_records ADD COLUMN batch_id TEXT`); err != nil {
			return fmt.Errorf("add batch_id column: %w", err)
		}
	}
	legacyIndex, err := s.tableIndexExists("release_records", true, "environment", "version")
	if err != nil {
		return err
	}
	if legacyIndex {
		rebuild := `
CREATE TABLE release_records_new (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  public_id      TEXT    NOT NULL UNIQUE,
  environment    TEXT    NOT NULL,
  version        TEXT    NOT NULL,
  gate_status    TEXT    NOT NULL,
  rollback_point TEXT    NOT NULL,
  batch_id       TEXT,
  recorded_at    TEXT    NOT NULL
);
INSERT INTO release_records_new
  (id, public_id, environment, version, gate_status, rollback_point, batch_id, recorded_at)
SELECT id, public_id, environment, version, gate_status, rollback_point, batch_id, recorded_at
FROM release_records;
DROP TABLE release_records;
ALTER TABLE release_records_new RENAME TO release_records;
`
		if _, err := s.db.Exec(rebuild); err != nil {
			return fmt.Errorf("rebuild release_records: %w", err)
		}
	}
	indexes := []string{
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_release_records_env_version
		 ON release_records(environment, version) WHERE batch_id IS NULL`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_release_records_batch
		 ON release_records(batch_id, environment, version) WHERE batch_id IS NOT NULL`,
	}
	for _, statement := range indexes {
		if _, err := s.db.Exec(statement); err != nil {
			return fmt.Errorf("create batch index: %w", err)
		}
	}
	return nil
}

// tableIndexExists reports whether the table has an index whose column
// prefix matches the requested columns, optionally limited to unique indexes.
func (s *Store) tableIndexExists(table string, uniqueOnly bool, columns ...string) (bool, error) {
	rows, err := s.db.Query(`PRAGMA index_list(` + table + `)`)
	if err != nil {
		return false, err
	}
	type indexInfo struct {
		name   string
		unique bool
		origin string
	}
	var indexes []indexInfo
	for rows.Next() {
		var seq, partial int
		var name, origin string
		var unique int
		if err := rows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			rows.Close()
			return false, err
		}
		indexes = append(indexes, indexInfo{name: name, unique: unique == 1, origin: origin})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	for _, index := range indexes {
		if uniqueOnly && (!index.unique || index.origin != "u") {
			continue
		}
		columnRows, err := s.db.Query(`PRAGMA index_info(` + index.name + `)`)
		if err != nil {
			return false, err
		}
		match := true
		position := 0
		for columnRows.Next() {
			var seqno, cid int
			var name string
			if err := columnRows.Scan(&seqno, &cid, &name); err != nil {
				columnRows.Close()
				return false, err
			}
			if position < len(columns) && name != columns[position] {
				match = false
			}
			position++
		}
		columnRows.Close()
		if err := columnRows.Err(); err != nil {
			return false, err
		}
		if match && position >= len(columns) {
			return true, nil
		}
	}
	return false, nil
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
