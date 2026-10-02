package store

import (
	"fmt"
	"strings"
)

// ChangeEntryFilter narrows ListChangeEntries to structured changes stored
// through POST /api/v1/release-records. Zero-valued fields are ignored;
// From/To are inclusive second-precision UTC bounds on the owning record's
// recorded_at.
type ChangeEntryFilter struct {
	Environment string
	Version     string
	BatchID     string
	Category    string
	Title       string
	GateStatus  string
	From        string
	To          string
}

// ChangeEntryAnchor is the keyset position of the last entry already
// returned. A zero anchor requests the first page.
type ChangeEntryAnchor struct {
	RecordedAt string
	RecordID   int64
	Sequence   int
	Title      string
	EntryID    int64
}

// ChangeEntryRecord is one structured change joined with the release it
// belongs to. It carries the internal ids and recorded_at needed to page
// deterministically and to build the next cursor.
type ChangeEntryRecord struct {
	ChangeEntry
	ID            int64
	RecordID      int64
	PublicID      string
	Environment   string
	Version       string
	BatchID       string
	GateStatus    string
	RollbackPoint string
	RecordedAt    string
}

// ListChangeEntries returns one page of structured change entries in the
// order the change-entries API exposes them: owning record recorded_at
// descending, insertion order of records descending, then entry sequence and
// title ascending. Up to limit+1 rows are fetched so the caller detects a
// following page. The inner join keeps changes attached only to v1 release
// records; baseline POST /releases rows never appear.
func (s *Store) ListChangeEntries(filter ChangeEntryFilter, anchor ChangeEntryAnchor, limit int) ([]ChangeEntryRecord, error) {
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
	if filter.Category != "" {
		where = append(where, `e.category = ?`)
		args = append(args, filter.Category)
	}
	if filter.Title != "" {
		where = append(where, `e.title = ?`)
		args = append(args, filter.Title)
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
	if anchor.RecordID > 0 {
		where = append(where, `(r.recorded_at < ?
			OR (r.recorded_at = ? AND r.id < ?)
			OR (r.recorded_at = ? AND r.id = ? AND (e.sequence_no > ?
				OR (e.sequence_no = ? AND (e.title > ?
					OR (e.title = ? AND e.id > ?))))))`)
		args = append(args,
			anchor.RecordedAt,
			anchor.RecordedAt, anchor.RecordID,
			anchor.RecordedAt, anchor.RecordID,
			anchor.Sequence,
			anchor.Sequence,
			anchor.Title,
			anchor.Title, anchor.EntryID,
		)
	}
	query := `SELECT e.id, e.sequence_no, e.category, e.title, e.description,
		 r.id, r.public_id, r.environment, r.version, r.batch_id, r.gate_status, r.rollback_point, r.recorded_at
		 FROM release_change_entries e
		 JOIN release_records r ON r.id = e.record_id`
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, ` AND `)
	}
	query += ` ORDER BY r.recorded_at DESC, r.id DESC, e.sequence_no ASC, e.title ASC, e.id ASC
		 LIMIT ?`
	args = append(args, limit+1)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query change entries: %w", err)
	}
	defer rows.Close()
	entries := []ChangeEntryRecord{}
	for rows.Next() {
		var entry ChangeEntryRecord
		if err := rows.Scan(
			&entry.ID, &entry.Sequence, &entry.Category, &entry.Title, &entry.Description,
			&entry.RecordID, &entry.PublicID, &entry.Environment, &entry.Version, &entry.BatchID,
			&entry.GateStatus, &entry.RollbackPoint, &entry.RecordedAt,
		); err != nil {
			return nil, fmt.Errorf("store: scan change entry: %w", err)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: query change entries: %w", err)
	}
	return entries, nil
}
