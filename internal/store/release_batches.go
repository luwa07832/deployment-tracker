package store

import (
	"fmt"
	"sort"
	"strings"
)

// ReleaseBatchFilter narrows the release-batch discovery queries. Empty
// fields are ignored; From/To are inclusive second-precision UTC bounds on
// recorded_at. A batch qualifies when at least one of its records satisfies
// every supplied condition.
type ReleaseBatchFilter struct {
	Environment string
	Version     string
	GateStatus  string
	From        string
	To          string
}

// ReleaseBatchGateCounts counts records of a batch by gate status.
type ReleaseBatchGateCounts struct {
	Allowed int `json:"allowed"`
	Blocked int `json:"blocked"`
	Pending int `json:"pending"`
}

// ReleaseBatchSummary is the read-side aggregate of one non-empty batch:
// every release record of the batch, not only the records that matched the
// discovery filter, feeds the counts.
type ReleaseBatchSummary struct {
	BatchID        string
	ReleaseCount   int
	Environments   []string
	ChangeCount    int
	GateCounts     ReleaseBatchGateCounts
	LastRecordedAt string
	Releases       []ReleaseRecord
}

// matchingBatchIDs returns the non-empty batch identifiers of every record
// satisfying all filter conditions. Batches qualify on at least one matching
// record; unbatched rows (batch_id = ”) never qualify.
func (s *Store) matchingBatchIDs(filter ReleaseBatchFilter) ([]string, error) {
	where := []string{`batch_id != ''`}
	args := []any{}
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
	query := `SELECT DISTINCT batch_id FROM release_records WHERE ` +
		strings.Join(where, ` AND `)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query release batch ids: %w", err)
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scan release batch id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: query release batch ids: %w", err)
	}
	return ids, nil
}

// ListReleaseBatches returns one keyset page of batch summaries matching
// filter in discovery order: last_recorded_at descending, batch_id
// descending to break ties. The anchor marks the last batch already
// returned; empty values request the first page. Up to limit+1 summaries are
// returned so the caller can detect a following page.
func (s *Store) ListReleaseBatches(
	filter ReleaseBatchFilter,
	anchorLastRecordedAt, anchorBatchID string,
	limit int,
) ([]ReleaseBatchSummary, error) {
	ids, err := s.matchingBatchIDs(filter)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return []ReleaseBatchSummary{}, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	records, err := s.queryReleaseRecords(
		`SELECT r.id, r.`+strings.ReplaceAll(releaseRecordColumns, ", ", ", r.")+
			`, e.sequence_no, e.category, e.title, e.description
		 FROM release_records r
		 LEFT JOIN release_change_entries e ON e.record_id = r.id
		 WHERE r.batch_id IN (`+placeholders+`)
		 ORDER BY r.batch_id ASC, r.recorded_at ASC, r.id ASC, e.sequence_no ASC, e.id ASC`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	summaries := buildReleaseBatchSummaries(records)
	sort.SliceStable(summaries, func(i, j int) bool {
		if summaries[i].LastRecordedAt != summaries[j].LastRecordedAt {
			return summaries[i].LastRecordedAt > summaries[j].LastRecordedAt
		}
		return summaries[i].BatchID > summaries[j].BatchID
	})
	if anchorBatchID != "" {
		offset := sort.Search(len(summaries), func(i int) bool {
			summary := summaries[i]
			return summary.LastRecordedAt < anchorLastRecordedAt ||
				(summary.LastRecordedAt == anchorLastRecordedAt && summary.BatchID < anchorBatchID)
		})
		summaries = summaries[offset:]
	}
	if len(summaries) > limit+1 {
		summaries = summaries[:limit+1]
	}
	return summaries, nil
}

// GetReleaseBatch returns the aggregate summary of one batch and every
// release record belonging to it. Releases are ordered oldest first
// (recorded_at ascending); records sharing a second break ties by internal
// id descending, matching write order newest-first within that second. It
// returns (nil, nil) when the batch has no records.
func (s *Store) GetReleaseBatch(batchID string) (*ReleaseBatchSummary, error) {
	if batchID == "" {
		return nil, nil
	}
	records, err := s.queryReleaseRecords(
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
	if len(records) == 0 {
		return nil, nil
	}
	summaries := buildReleaseBatchSummaries(records)
	if len(summaries) != 1 {
		return nil, fmt.Errorf("store: unexpected batch summary count %d", len(summaries))
	}
	return &summaries[0], nil
}

// buildReleaseBatchSummaries groups release records by batch_id and folds
// every record of each batch into one summary. Input records are expected in
// an order that keeps each record's change entries contiguous; the returned
// Releases preserve the input order.
func buildReleaseBatchSummaries(records []ReleaseRecord) []ReleaseBatchSummary {
	order := make([]string, 0)
	byID := map[string]*ReleaseBatchSummary{}
	for i := range records {
		record := records[i]
		if record.BatchID == "" {
			continue
		}
		summary, seen := byID[record.BatchID]
		if !seen {
			summary = &ReleaseBatchSummary{
				BatchID:      record.BatchID,
				Environments: []string{},
				Releases:     []ReleaseRecord{},
			}
			byID[record.BatchID] = summary
			order = append(order, record.BatchID)
		}
		summary.ReleaseCount++
		summary.ChangeCount += len(record.Changes)
		switch record.GateStatus {
		case "allowed":
			summary.GateCounts.Allowed++
		case "blocked":
			summary.GateCounts.Blocked++
		case "pending":
			summary.GateCounts.Pending++
		}
		if record.RecordedAt > summary.LastRecordedAt {
			summary.LastRecordedAt = record.RecordedAt
		}
		summary.Releases = append(summary.Releases, record)
	}
	summaries := make([]ReleaseBatchSummary, 0, len(order))
	for _, id := range order {
		summary := byID[id]
		envSet := map[string]bool{}
		for i := range summary.Releases {
			envSet[summary.Releases[i].Environment] = true
		}
		environments := make([]string, 0, len(envSet))
		for env := range envSet {
			environments = append(environments, env)
		}
		sort.Strings(environments)
		summary.Environments = environments
		summaries = append(summaries, *summary)
	}
	return summaries
}
