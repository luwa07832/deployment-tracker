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
	PublicID      string
	Environment   string
	Version       string
	GateStatus    string
	RollbackPoint string
	BatchID       string
	Changes       []ChangeEntry
	RecordedAt    string
}

// ReleaseRecordFilter narrows ListReleaseRecords. Zero-valued fields are
// ignored; From/To are inclusive second-precision UTC bounds.
type ReleaseRecordFilter struct {
	Environment string
	Version     string
	GateStatus  string
	BatchID     string
	From        string
	To          string
}

const releaseRecordColumns = `public_id, environment, version, gate_status, rollback_point, batch_id, recorded_at`

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
	const maxAttempts = 3
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		id, err := newPublicID()
		if err != nil {
			return fmt.Errorf("store: new release id: %w", err)
		}
		var batchArg any
		if record.BatchID != "" {
			batchArg = record.BatchID
		}
		var recordedArg any
		if record.RecordedAt != "" {
			recordedArg = record.RecordedAt
		}
		res, err := s.db.Exec(
			`INSERT INTO release_records (public_id, environment, version, gate_status, rollback_point, batch_id, recorded_at)
			 VALUES (?, ?, ?, ?, ?, ?, COALESCE(?, strftime('%Y-%m-%dT%H:%M:%SZ','now')))`,
			id, record.Environment, record.Version, record.GateStatus, record.RollbackPoint, batchArg, recordedArg,
		)
		if err != nil {
			if isUniqueViolation(err) {
				collision, collisionErr := s.publicIDExists(id)
				if collisionErr != nil {
					return collisionErr
				}
				if collision {
					lastErr = err
					continue
				}
				return &ErrReleaseAlreadyExists{Environment: record.Environment, Version: record.Version}
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
		`SELECT r.`+strings.ReplaceAll(releaseRecordColumns, ", ", ", r.")+
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
	if filter.GateStatus != "" {
		where = append(where, `r.gate_status = ?`)
		args = append(args, filter.GateStatus)
	}
	if filter.BatchID != "" {
		where = append(where, `r.batch_id = ?`)
		args = append(args, filter.BatchID)
	}
	if filter.From != "" {
		where = append(where, `r.recorded_at >= ?`)
		args = append(args, filter.From)
	}
	if filter.To != "" {
		where = append(where, `r.recorded_at <= ?`)
		args = append(args, filter.To)
	}
	query := `SELECT r.` + strings.ReplaceAll(releaseRecordColumns, ", ", ", r.") +
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
		`SELECT r.`+strings.ReplaceAll(releaseRecordColumns, ", ", ", r.")+
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
		var category, title, description, batchID sql.NullString
		if err := rows.Scan(
			&record.PublicID, &record.Environment, &record.Version, &record.GateStatus,
			&record.RollbackPoint, &batchID, &record.RecordedAt,
			&sequence, &category, &title, &description,
		); err != nil {
			return nil, fmt.Errorf("store: scan release record: %w", err)
		}
		record.BatchID = batchID.String
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

// publicIDExists reports whether a record already carries the given public id.
func (s *Store) publicIDExists(publicID string) (bool, error) {
	var count int
	if err := s.db.QueryRow(
		`SELECT count(1) FROM release_records WHERE public_id = ?`, publicID,
	).Scan(&count); err != nil {
		return false, fmt.Errorf("store: check release record id: %w", err)
	}
	return count > 0, nil
}

// BatchExists reports whether at least one release record carries the batch id.
func (s *Store) BatchExists(batchID string) (bool, error) {
	var count int
	if err := s.db.QueryRow(
		`SELECT count(1) FROM release_records WHERE batch_id = ?`, batchID,
	).Scan(&count); err != nil {
		return false, fmt.Errorf("store: check release batch: %w", err)
	}
	return count > 0, nil
}

// ListBatchReleaseRecords returns every record tagged with the batch id in
// chronological (oldest first) order; equal timestamps break ties by the
// stable insertion order so repeated queries are deterministic.
func (s *Store) ListBatchReleaseRecords(batchID string) ([]ReleaseRecord, error) {
	return s.queryReleaseRecords(
		`SELECT r.`+strings.ReplaceAll(releaseRecordColumns, ", ", ", r.")+
			`, e.sequence_no, e.category, e.title, e.description
		 FROM release_records r
		 LEFT JOIN release_change_entries e ON e.record_id = r.id
		 WHERE r.batch_id = ?
		 ORDER BY r.recorded_at ASC, r.id ASC, e.sequence_no ASC, e.id ASC`,
		batchID,
	)
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
