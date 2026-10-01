package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// Release is one immutable release record. Records created before change
// tracking was added carry nil Changes, GateStatus and RollbackPoint; readers
// must treat those as "field not present" rather than invalid.
type Release struct {
	ID            int64
	Name          string
	Environment   string
	Version       string
	Changes       []string
	GateStatus    *string
	RollbackPoint *string
	CreatedAt     string
}

// ReleaseFilter narrows ListReleases. The zero value matches every record.
type ReleaseFilter struct {
	Environment   string
	Version       string
	Change        string
	GateStatus    string
	RollbackPoint string
}

const releaseColumns = `id, name, environment, version, changes_json, gate_status, rollback_point, created_at`

// InsertRelease stores a new release record and fills in ID and CreatedAt.
func (s *Store) InsertRelease(rel *Release) error {
	changesJSON, err := json.Marshal(rel.Changes)
	if err != nil {
		return fmt.Errorf("store: encode changes: %w", err)
	}
	res, err := s.db.Exec(
		`INSERT INTO deployments (name, environment, version, changes_json, gate_status, rollback_point)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		rel.Name, rel.Environment, rel.Version, string(changesJSON), rel.GateStatus, rel.RollbackPoint,
	)
	if err != nil {
		return fmt.Errorf("store: insert release: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("store: insert release: %w", err)
	}
	rel.ID = id
	return s.db.QueryRow(`SELECT created_at FROM deployments WHERE id = ?`, id).Scan(&rel.CreatedAt)
}

// FindRelease returns the effective record for an environment and version: the
// one registered last. It returns (nil, nil) when no such record exists.
func (s *Store) FindRelease(environment, version string) (*Release, error) {
	row := s.db.QueryRow(
		`SELECT `+releaseColumns+` FROM deployments
		 WHERE environment = ? AND version = ?
		 ORDER BY id DESC LIMIT 1`,
		environment, version,
	)
	rel, err := scanRelease(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: find release: %w", err)
	}
	return rel, nil
}

// ListReleases returns every record matching the filter in registration order
// (oldest first). An empty result is an empty slice, never nil.
func (s *Store) ListReleases(filter ReleaseFilter) ([]Release, error) {
	var where []string
	var args []any
	if filter.Environment != "" {
		where = append(where, `environment = ?`)
		args = append(args, filter.Environment)
	}
	if filter.Version != "" {
		where = append(where, `version = ?`)
		args = append(args, filter.Version)
	}
	if filter.Change != "" {
		where = append(where, `changes_json IS NOT NULL AND EXISTS (SELECT 1 FROM json_each(deployments.changes_json) WHERE json_each.value = ?)`)
		args = append(args, filter.Change)
	}
	if filter.GateStatus != "" {
		where = append(where, `gate_status = ?`)
		args = append(args, filter.GateStatus)
	}
	if filter.RollbackPoint != "" {
		where = append(where, `rollback_point = ?`)
		args = append(args, filter.RollbackPoint)
	}
	query := `SELECT ` + releaseColumns + ` FROM deployments`
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, ` AND `)
	}
	query += ` ORDER BY id ASC`
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list releases: %w", err)
	}
	defer rows.Close()
	releases := []Release{}
	for rows.Next() {
		rel, err := scanRelease(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list releases: %w", err)
		}
		releases = append(releases, *rel)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list releases: %w", err)
	}
	return releases, nil
}

// EnvironmentExists reports whether any release record names the environment.
func (s *Store) EnvironmentExists(environment string) (bool, error) {
	var count int
	if err := s.db.QueryRow(`SELECT count(1) FROM deployments WHERE environment = ?`, environment).Scan(&count); err != nil {
		return false, fmt.Errorf("store: check environment: %w", err)
	}
	return count > 0, nil
}

// HistoryPage is one stable page of an environment's release history, newest
// first. Records sharing a created_at timestamp break ties by registration
// id, so paging forward never repeats or skips a row, even when new releases
// are registered while a caller pages.
type HistoryPage struct {
	Releases   []Release
	HasNext    bool
	NextLastID int64
	NextLastAt string
}

// ListReleaseHistory returns one keyset page for one environment, ordered by
// created_at DESC and id DESC. The page carries up to limit releases; when
// more rows follow, the page also carries the opaque continuation position
// for the next request. An empty zero-id cursor starts at the newest release.
func (s *Store) ListReleaseHistory(environment string, limit int, afterID int64, afterAt string) (HistoryPage, error) {
	var (
		releases []Release
		args     []any
	)
	query := `SELECT ` + releaseColumns + ` FROM deployments WHERE environment = ?`
	args = append(args, environment)
	if afterID > 0 {
		// The cursor points at the last row already returned; keep only rows
		// strictly earlier in the (created_at, id) ordering.
		query += ` AND (created_at < ? OR (created_at = ? AND id < ?))`
		args = append(args, afterAt, afterAt, afterID)
	}
	query += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return HistoryPage{}, fmt.Errorf("store: list release history: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		rel, err := scanRelease(rows)
		if err != nil {
			return HistoryPage{}, fmt.Errorf("store: list release history: %w", err)
		}
		releases = append(releases, *rel)
	}
	if err := rows.Err(); err != nil {
		return HistoryPage{}, fmt.Errorf("store: list release history: %w", err)
	}
	page := HistoryPage{Releases: []Release{}}
	if len(releases) > limit {
		page.HasNext = true
		releases = releases[:limit]
	}
	page.Releases = releases
	if page.HasNext {
		last := releases[len(releases)-1]
		page.NextLastID = last.ID
		page.NextLastAt = last.CreatedAt
	}
	return page, nil
}

// scanRelease reads one row from anything that behaves like sql.Row or sql.Rows.
func scanRelease(row interface{ Scan(...any) error }) (*Release, error) {
	var rel Release
	var changesJSON, gateStatus, rollbackPoint sql.NullString
	err := row.Scan(&rel.ID, &rel.Name, &rel.Environment, &rel.Version, &changesJSON, &gateStatus, &rollbackPoint, &rel.CreatedAt)
	if err != nil {
		return nil, err
	}
	if changesJSON.Valid {
		var changes []string
		if err := json.Unmarshal([]byte(changesJSON.String), &changes); err != nil {
			return nil, fmt.Errorf("store: decode changes: %w", err)
		}
		rel.Changes = changes
	}
	if gateStatus.Valid {
		value := gateStatus.String
		rel.GateStatus = &value
	}
	if rollbackPoint.Valid {
		value := rollbackPoint.String
		rel.RollbackPoint = &value
	}
	return &rel, nil
}
