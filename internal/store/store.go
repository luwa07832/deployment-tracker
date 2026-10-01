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
	return nil
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
