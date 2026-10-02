package store

import (
	"fmt"
	"strings"
)

// ReleaseBatchFilter narrows the release-batch discovery queries. Empty
// fields are ignored; From/To are inclusive second-precision UTC bounds on
// recorded_at. Every condition applies to individual records with AND
// semantics: a batch qualifies once it owns at least one non-empty batch_id
// record matching every condition.
type ReleaseBatchFilter struct {
	Environment string
	Version     string
	GateStatus  string
	From        string
	To          string
}

// ReleaseBatchSummary is the batch-wide picture built from every release
// record carrying the batch identifier, not only the records matching the
// discovery filter.
type ReleaseBatchSummary struct {
	BatchID        string
	ReleaseCount   int
	ChangeCount    int
	GateAllowed    int
	GateBlocked    int
	GatePending    int
	LastRecordedAt string
}

// batchQualifyingClause builds the WHERE fragment that selects the batch ids
// owning at least one matching non-empty batch_id record. The returned
// arguments bind to the placeholders in fragment order.
func batchQualifyingClause(filter ReleaseBatchFilter) (string, []any) {
	var where []string
	var args []any
	where = append(where, `batch_id <> ''`)
	if filter.Environment != "" {
		where = append(where, `environment = ?`)
		args = append(args, filter.Environment)
	}
	if filter.Version != "" {
		where = append(where, `version = ?`)
		args = append(args, filter.Version)
	}
	if filter.GateStatus != "" {
		where = append(where, `gate_status = ?`)
		args = append(args, filter.GateStatus)
	}
	if filter.From != "" {
		where = append(where, `recorded_at >= ?`)
		args = append(args, filter.From)
	}
	if filter.To != "" {
		where = append(where, `recorded_at <= ?`)
		args = append(args, filter.To)
	}
	return `SELECT DISTINCT batch_id FROM release_records WHERE ` + strings.Join(where, ` AND `), args
}

const releaseBatchSummarySelect = `
       r.batch_id AS batch_id,
       count(DISTINCT r.id) AS release_count,
       (SELECT count(1) FROM release_change_entries e
        JOIN release_records er ON er.id = e.record_id
        WHERE er.batch_id = r.batch_id) AS change_count,
       count(DISTINCT CASE WHEN r.gate_status = 'allowed' THEN r.id END) AS gate_allowed,
       count(DISTINCT CASE WHEN r.gate_status = 'blocked' THEN r.id END) AS gate_blocked,
       count(DISTINCT CASE WHEN r.gate_status = 'pending' THEN r.id END) AS gate_pending,
       max(r.recorded_at) AS last_recorded_at`

func scanReleaseBatchSummary(rows interface {
	Scan(dest ...any) error
}) (ReleaseBatchSummary, error) {
	var summary ReleaseBatchSummary
	if err := rows.Scan(
		&summary.BatchID, &summary.ReleaseCount, &summary.ChangeCount,
		&summary.GateAllowed, &summary.GateBlocked, &summary.GatePending,
		&summary.LastRecordedAt,
	); err != nil {
		return ReleaseBatchSummary{}, fmt.Errorf("store: scan release batch summary: %w", err)
	}
	return summary, nil
}

// ListReleaseBatchesPage returns one keyset page of batch summaries matching
// filter, ordered newest first (last_recorded_at descending, batch_id
// descending for ties). The anchor marks the last batch already returned;
// empty anchor values request the first page. Up to limit+1 summaries are
// fetched so the caller can detect a following page. Summaries cover every
// record of each qualifying batch, including records outside the filter.
func (s *Store) ListReleaseBatchesPage(
	filter ReleaseBatchFilter,
	anchorRecordedAt string,
	anchorBatchID string,
	limit int,
) ([]ReleaseBatchSummary, error) {
	qualifying, qualifyingArgs := batchQualifyingClause(filter)
	query := `SELECT` + releaseBatchSummarySelect + `
		  FROM release_records r
		 WHERE r.batch_id IN (` + qualifying + `)`
	args := append([]any{}, qualifyingArgs...)
	if anchorBatchID != "" {
		query += `
		 GROUP BY r.batch_id
		HAVING max(r.recorded_at) < ?
		    OR (max(r.recorded_at) = ? AND r.batch_id < ?)`
		args = append(args, anchorRecordedAt, anchorRecordedAt, anchorBatchID)
	} else {
		query += `
		 GROUP BY r.batch_id`
	}
	query += `
		 ORDER BY last_recorded_at DESC, batch_id DESC
		 LIMIT ?`
	args = append(args, limit+1)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query release batches: %w", err)
	}
	defer rows.Close()
	summaries := []ReleaseBatchSummary{}
	for rows.Next() {
		summary, err := scanReleaseBatchSummary(rows)
		if err != nil {
			return nil, err
		}
		summaries = append(summaries, summary)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: query release batches: %w", err)
	}
	return summaries, nil
}

// GetReleaseBatchSummary returns the batch-wide summary for one exact batch
// identifier, or (nil, nil) when no release record carries it.
func (s *Store) GetReleaseBatchSummary(batchID string) (*ReleaseBatchSummary, error) {
	if batchID == "" {
		return nil, nil
	}
	query := `SELECT` + releaseBatchSummarySelect + `
		  FROM release_records r
		 WHERE r.batch_id = ?
		 GROUP BY r.batch_id`
	rows, err := s.db.Query(query, batchID)
	if err != nil {
		return nil, fmt.Errorf("store: query release batch summary: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, nil
	}
	summary, err := scanReleaseBatchSummary(rows)
	if err != nil {
		return nil, err
	}
	return &summary, nil
}

// ListReleaseBatchEnvironments returns the environments of every record of a
// batch, deduplicated and sorted ascending.
func (s *Store) ListReleaseBatchEnvironments(batchID string) ([]string, error) {
	if batchID == "" {
		return []string{}, nil
	}
	rows, err := s.db.Query(
		`SELECT DISTINCT environment FROM release_records
		 WHERE batch_id = ? ORDER BY environment ASC`,
		batchID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: query release batch environments: %w", err)
	}
	defer rows.Close()
	environments := []string{}
	for rows.Next() {
		var environment string
		if err := rows.Scan(&environment); err != nil {
			return nil, fmt.Errorf("store: scan release batch environment: %w", err)
		}
		environments = append(environments, environment)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: query release batch environments: %w", err)
	}
	return environments, nil
}

// ListReleaseBatchRecords returns every release record of one batch in
// chronological order (recorded_at ascending); records sharing a recorded_at
// second break ties by internal id descending, matching the requested
// same-instant ordering. Each record carries its change entries in stored
// order.
func (s *Store) ListReleaseBatchRecords(batchID string) ([]ReleaseRecord, error) {
	if batchID == "" {
		return []ReleaseRecord{}, nil
	}
	rows, err := s.queryReleaseRecords(
		`SELECT r.id, r.`+strings.ReplaceAll(releaseRecordColumns, ", ", ", r.")+
			`, e.sequence_no, e.category, e.title, e.description
		 FROM release_records r
		 LEFT JOIN release_change_entries e ON e.record_id = r.id
		 WHERE r.batch_id = ?
		 ORDER BY r.recorded_at ASC, r.id DESC, e.sequence_no ASC, e.id ASC`,
		batchID,
	)
	if err != nil {
		return nil, err
	}
	return rows, nil
}
