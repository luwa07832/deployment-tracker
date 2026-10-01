package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
)

// ChangeEntry is one structured change attached to a release record.
type ChangeEntry struct {
	Sequence    int    `json:"sequence"`
	Category    string `json:"category"`
	Title       string `json:"title"`
	Description string `json:"description"`
}

// ReleaseRecord is one traceable release record with structured change
// entries and an opaque rollback point identifier.
type ReleaseRecord struct {
	ID            int64
	PublicID      string
	Environment   string
	Version       string
	BatchID       string
	GateStatus    string
	RollbackPoint string
	Changes       []ChangeEntry
	RecordedAt    string
}

// ReleaseRecordFilter narrows ListReleaseRecords. Zero-valued fields are
// ignored; From/To are inclusive second-precision UTC bounds.
type ReleaseRecordFilter struct {
	Environment string
	Version     string
	BatchID     string
	GateStatus  string
	From        string
	To          string
}

const releaseRecordColumns = `public_id, environment, version, batch_id, gate_status, rollback_point, recorded_at`

// ErrReleaseAlreadyExists marks a duplicate (environment, version) submission.
type ErrReleaseAlreadyExists struct {
	Environment string
	Version     string
}

func (e *ErrReleaseAlreadyExists) Error() string {
	return "release record " + e.Environment + " " + e.Version + " already exists"
}

// InsertReleaseRecord stores one release record, generating the stable public
// identifier and server-side recorded_at. A duplicate (environment, version)
// pair returns *ErrReleaseAlreadyExists.
func (s *Store) InsertReleaseRecord(record *ReleaseRecord) error {
	// The batch-aware partial indexes keep unbatched rows unique on
	// (environment, version) and batched rows unique per batch. Rows of the
	// other batch flavor sharing the same environment and version still
	// represent the same single effective release, so they conflict too.
	var existing int
	if err := s.db.QueryRow(
		`SELECT count(1) FROM release_records
		 WHERE environment = ? AND version = ?
		   AND (batch_id = '' OR ? = '')
		   AND batch_id != ?`,
		record.Environment, record.Version, record.BatchID, record.BatchID,
	).Scan(&existing); err != nil {
		return fmt.Errorf("store: check release record: %w", err)
	}
	if existing > 0 {
		return &ErrReleaseAlreadyExists{Environment: record.Environment, Version: record.Version}
	}
	const maxAttempts = 3
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		id, err := newPublicID()
		if err != nil {
			return fmt.Errorf("store: new release id: %w", err)
		}
		res, err := s.db.Exec(
			`INSERT INTO release_records (public_id, environment, version, batch_id, gate_status, rollback_point)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			id, record.Environment, record.Version, record.BatchID, record.GateStatus, record.RollbackPoint,
		)
		if err != nil {
			if isUniqueViolation(err) {
				message := strings.ToLower(err.Error())
				if strings.Contains(message, "release_records.environment") ||
					strings.Contains(message, "release_records_env_version") {
					return &ErrReleaseAlreadyExists{Environment: record.Environment, Version: record.Version}
				}
				lastErr = err
				continue
			}
			return fmt.Errorf("store: insert release record: %w", err)
		}
		rowID, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("store: insert release record: %w", err)
		}
		for i := range record.Changes {
			if _, err := s.db.Exec(
				`INSERT INTO release_change_entries (record_id, sequence_no, category, title, description)
				 VALUES (?, ?, ?, ?, ?)`,
				rowID, record.Changes[i].Sequence, record.Changes[i].Category,
				record.Changes[i].Title, record.Changes[i].Description,
			); err != nil {
				return fmt.Errorf("store: insert change entry: %w", err)
			}
		}
		stored, err := s.GetReleaseRecord(id)
		if err != nil {
			return err
		}
		*record = *stored
		return nil
	}
	return fmt.Errorf("store: insert release record: %w", lastErr)
}

// GetReleaseRecord returns one record by its stable public identifier, or
// (nil, nil) when it does not exist.
func (s *Store) GetReleaseRecord(publicID string) (*ReleaseRecord, error) {
	rows, err := s.queryReleaseRecords(
		`SELECT r.id, r.`+strings.ReplaceAll(releaseRecordColumns, ", ", ", r.")+
			`, e.sequence_no, e.category, e.title, e.description
		 FROM release_records r
		 LEFT JOIN release_change_entries e ON e.record_id = r.id
		 WHERE r.public_id = ?
		 ORDER BY r.id DESC, e.sequence_no ASC, e.id ASC`,
		publicID,
	)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

// ListReleaseRecords returns matching records newest first. Records sharing a
// recorded_at timestamp break ties by their internal id descending, keeping
// the order deterministic and identical to registration order.
func (s *Store) ListReleaseRecords(filter ReleaseRecordFilter) ([]ReleaseRecord, error) {
	var where []string
	var args []any
	if filter.Environment != "" {
		where = append(where, `r.environment = ?`)
		args = append(args, filter.Environment)
	}
	if filter.Version != "" {
		where = append(where, `r.version = ?`)
		args = append(args, filter.Version)
	}
	if filter.BatchID != "" {
		where = append(where, `r.batch_id = ?`)
		args = append(args, filter.BatchID)
	}
	if filter.GateStatus != "" {
		where = append(where, `r.gate_status = ?`)
		args = append(args, filter.GateStatus)
	}
	if filter.From != "" {
		where = append(where, `r.recorded_at >= ?`)
		args = append(args, filter.From)
	}
	if filter.To != "" {
		where = append(where, `r.recorded_at <= ?`)
		args = append(args, filter.To)
	}
	query := `SELECT r.id, r.` + strings.ReplaceAll(releaseRecordColumns, ", ", ", r.") +
		`, e.sequence_no, e.category, e.title, e.description
		 FROM release_records r
		 LEFT JOIN release_change_entries e ON e.record_id = r.id`
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, ` AND `)
	}
	query += ` ORDER BY r.recorded_at DESC, r.id DESC, e.sequence_no ASC, e.id ASC`
	return s.queryReleaseRecords(query, args...)
}

// EffectiveReleaseRecord returns the newest record for an environment and
// version, or (nil, nil) when absent. The (environment, version) pair is
// unique for new records, but this keeps the same "latest wins" read
// semantics the baseline API uses.
func (s *Store) EffectiveReleaseRecord(environment, version string) (*ReleaseRecord, error) {
	rows, err := s.queryReleaseRecords(
		`SELECT r.id, r.`+strings.ReplaceAll(releaseRecordColumns, ", ", ", r.")+
			`, e.sequence_no, e.category, e.title, e.description
		 FROM release_records r
		 LEFT JOIN release_change_entries e ON e.record_id = r.id
		 WHERE r.environment = ? AND r.version = ?
		 ORDER BY r.id DESC, e.sequence_no ASC, e.id ASC`,
		environment, version,
	)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

// ListReleaseRecordHistory returns one page of an environment's records in
// the order history pages follow: recorded_at descending, then insertion id
// descending. The keyset anchor (afterRecordedAt, afterID) marks the last row
// already returned; zero values request the first page. Up to limit+1 rows are
// fetched so the caller can detect a following page.
func (s *Store) ListReleaseRecordHistory(environment string, afterRecordedAt string, afterID int64, limit int) ([]ReleaseRecord, error) {
	query := `SELECT r.id, r.` + strings.ReplaceAll(releaseRecordColumns, ", ", ", r.") +
		`, e.sequence_no, e.category, e.title, e.description
		 FROM (
		   SELECT id, recorded_at FROM release_records
		   WHERE environment = ?`
	args := []any{environment}
	if afterID > 0 {
		query += ` AND (recorded_at < ? OR (recorded_at = ? AND id < ?))`
		args = append(args, afterRecordedAt, afterRecordedAt, afterID)
	}
	query += ` ORDER BY recorded_at DESC, id DESC LIMIT ?
		 ) AS page
		 JOIN release_records r ON r.id = page.id
		 LEFT JOIN release_change_entries e ON e.record_id = r.id
		 ORDER BY page.recorded_at DESC, page.id DESC, e.sequence_no ASC, e.id ASC`
	args = append(args, limit+1)
	return s.queryReleaseRecords(query, args...)
}

// queryReleaseRecords runs a joined query and assembles records with their
// change entries. Rows must arrive ordered record (descending) then entry.
func (s *Store) queryReleaseRecords(query string, args ...any) ([]ReleaseRecord, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query release records: %w", err)
	}
	defer rows.Close()
	records := []ReleaseRecord{}
	indexByID := map[string]int{}
	for rows.Next() {
		var record ReleaseRecord
		var sequence sql.NullInt64
		var category, title, description sql.NullString
		if err := rows.Scan(
			&record.ID, &record.PublicID, &record.Environment, &record.Version, &record.BatchID, &record.GateStatus,
			&record.RollbackPoint, &record.RecordedAt,
			&sequence, &category, &title, &description,
		); err != nil {
			return nil, fmt.Errorf("store: scan release record: %w", err)
		}
		pos, seen := indexByID[record.PublicID]
		if !seen {
			record.Changes = []ChangeEntry{}
			records = append(records, record)
			pos = len(records) - 1
			indexByID[record.PublicID] = pos
		}
		if sequence.Valid {
			records[pos].Changes = append(records[pos].Changes, ChangeEntry{
				Sequence:    int(sequence.Int64),
				Category:    category.String,
				Title:       title.String,
				Description: description.String,
			})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: query release records: %w", err)
	}
	return records, nil
}

// newPublicID returns an opaque stable identifier for a release record.
func newPublicID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "rel_" + hex.EncodeToString(buf), nil
}

// isUniqueViolation reports whether err is a SQLite UNIQUE constraint failure.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "constraint failed") && strings.Contains(message, "unique")
}

// ReleaseBatchExists reports whether at least one release record is stored
// for the given batch identifier.
func (s *Store) ReleaseBatchExists(batchID string) (bool, error) {
	var count int
	if err := s.db.QueryRow(
		`SELECT count(1) FROM release_records WHERE batch_id = ?`, batchID,
	).Scan(&count); err != nil {
		return false, fmt.Errorf("store: check release batch: %w", err)
	}
	return count > 0, nil
}
