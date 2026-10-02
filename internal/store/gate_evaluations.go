package store

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// Gate evaluation error codes returned verbatim in API error responses.
const (
	CodeGateEvaluationConflictV1    = "GATE_EVALUATION_CONFLICT"
	CodeGateEvaluationValidationV1  = "GATE_EVALUATION_VALIDATION_FAILED"
	CodeGateEvaluationNotFoundV1    = "GATE_EVALUATION_NOT_FOUND"
	CodeGateStatusMismatchV1        = "GATE_STATUS_MISMATCH"
	CodeInvalidGateEvaluationQueryV = "INVALID_GATE_EVALUATION_QUERY"
)

// GateCheck is one immutable public gate check captured in a snapshot.
type GateCheck struct {
	Name         string
	Status       string
	Evidence     string
	WaiverReason string
}

// GateEvaluation is the immutable public gate-check snapshot explaining a
// release record's gate_status. Checks are stored and returned ordered by
// check_name.
type GateEvaluation struct {
	RecordID            int64
	PublicID            string
	Environment         string
	Version             string
	BatchID             string
	GateStatus          string
	EffectiveGateStatus string
	Checks              []GateCheck
	RecordedAt          string
}

// GateEvaluationFilter narrows ListGateEvaluationsPage. Empty fields are
// ignored; CheckStatus keeps snapshots that contain at least one check with
// that status.
type GateEvaluationFilter struct {
	Environment string
	Version     string
	GateStatus  string
	CheckStatus string
}

// ErrReleaseRecordMissing marks a save against a public id that does not
// identify a release record.
type ErrReleaseRecordMissing struct {
	PublicID string
}

func (e *ErrReleaseRecordMissing) Error() string {
	return "release record " + e.PublicID + " not found"
}

// ErrGateEvaluationConflict marks a second, differently-content snapshot
// submitted for the same release record. Snapshots are immutable.
type ErrGateEvaluationConflict struct {
	PublicID string
}

func (e *ErrGateEvaluationConflict) Error() string {
	return "gate evaluation for release record " + e.PublicID + " already exists with different checks"
}

// ErrGateStatusMismatch marks that the checks derive a different gate
// decision than the release record's stored gate_status.
type ErrGateStatusMismatch struct {
	PublicID string
	Recorded string
	Derived  string
}

func (e *ErrGateStatusMismatch) Error() string {
	return "gate evaluation for release record " + e.PublicID + " derives " + e.Derived + " but the record stores " + e.Recorded
}

// DeriveGateStatus reduces gate check statuses to the release decision:
// any failed check blocks, otherwise any pending check leaves the gate
// pending, otherwise the release is allowed.
func DeriveGateStatus(checks []GateCheck) string {
	hasPending := false
	for _, check := range checks {
		switch check.Status {
		case GateCheckFailed:
			return "blocked"
		case GateCheckPending:
			hasPending = true
		}
	}
	if hasPending {
		return "pending"
	}
	return "allowed"
}

// Gate check statuses accepted by the API.
const (
	GateCheckPassed  = "passed"
	GateCheckFailed  = "failed"
	GateCheckWaived  = "waived"
	GateCheckPending = "pending"
)

// gateEvaluationInsertFaultHook, when set, runs inside the save transaction
// after the snapshot row is written and before the transaction commits. It
// exists for tests that force a mid-write failure and verify rollback.
var gateEvaluationInsertFaultHook func(publicID string, recordID int64) error

// SetGateEvaluationInsertFaultHookForTest installs a hook that can fail the
// save transaction after the snapshot row was written. It returns a restore
// function. Test-only.
func SetGateEvaluationInsertFaultHookForTest(hook func(publicID string, recordID int64) error) func() {
	previous := gateEvaluationInsertFaultHook
	gateEvaluationInsertFaultHook = hook
	return func() { gateEvaluationInsertFaultHook = previous }
}

// SaveGateEvaluation atomically stores one immutable gate-check snapshot for
// the release record identified by publicID. The snapshot is validated by
// the caller; the store derives the effective gate status and enforces that
// it matches the record's gate_status. Re-submitting identical normalized
// checks returns the existing snapshot with created == false; different
// checks return *ErrGateEvaluationConflict.
func (s *Store) SaveGateEvaluation(publicID string, checks []GateCheck) (evaluation *GateEvaluation, created bool, err error) {
	ordered := make([]GateCheck, len(checks))
	copy(ordered, checks)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Name < ordered[j].Name })

	tx, err := s.db.Begin()
	if err != nil {
		return nil, false, fmt.Errorf("store: begin gate evaluation tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	var recordID int64
	var environment, version, batchID, recordedGateStatus, recordedAt string
	err = tx.QueryRow(
		`SELECT id, environment, version, batch_id, gate_status, recorded_at
		 FROM release_records WHERE public_id = ?`,
		publicID,
	).Scan(&recordID, &environment, &version, &batchID, &recordedGateStatus, &recordedAt)
	if err == sql.ErrNoRows {
		return nil, false, &ErrReleaseRecordMissing{PublicID: publicID}
	}
	if err != nil {
		return nil, false, fmt.Errorf("store: load release record for gate evaluation: %w", err)
	}

	derived := DeriveGateStatus(ordered)
	if derived != recordedGateStatus {
		return nil, false, &ErrGateStatusMismatch{
			PublicID: publicID, Recorded: recordedGateStatus, Derived: derived,
		}
	}

	var existingID int64
	var existingEffective string
	lookupErr := tx.QueryRow(
		`SELECT id, effective_gate_status FROM release_gate_evaluations WHERE record_id = ?`,
		recordID,
	).Scan(&existingID, &existingEffective)
	if lookupErr != nil && lookupErr != sql.ErrNoRows {
		return nil, false, fmt.Errorf("store: look up gate evaluation: %w", lookupErr)
	}
	if lookupErr == nil {
		existingChecks, err := loadGateChecksTx(tx, existingID)
		if err != nil {
			return nil, false, err
		}
		if !gateChecksEqual(existingChecks, ordered) {
			return nil, false, &ErrGateEvaluationConflict{PublicID: publicID}
		}
		if err := tx.Commit(); err != nil {
			return nil, false, fmt.Errorf("store: commit gate evaluation: %w", err)
		}
		committed = true
		evaluation, err := s.GetGateEvaluation(publicID)
		if err != nil {
			return nil, false, err
		}
		return evaluation, false, nil
	}

	result, err := tx.Exec(
		`INSERT INTO release_gate_evaluations (record_id, gate_status, effective_gate_status)
		 VALUES (?, ?, ?)`,
		recordID, recordedGateStatus, derived,
	)
	if err != nil {
		return nil, false, fmt.Errorf("store: insert gate evaluation: %w", err)
	}
	evaluationID, err := result.LastInsertId()
	if err != nil {
		return nil, false, fmt.Errorf("store: insert gate evaluation: %w", err)
	}
	for _, check := range ordered {
		if _, err := tx.Exec(
			`INSERT INTO release_gate_checks
			 (evaluation_id, check_name, check_status, evidence, waiver_reason)
			 VALUES (?, ?, ?, ?, ?)`,
			evaluationID, check.Name, check.Status, check.Evidence, check.WaiverReason,
		); err != nil {
			return nil, false, fmt.Errorf("store: insert gate check: %w", err)
		}
	}
	if gateEvaluationInsertFaultHook != nil {
		if err := gateEvaluationInsertFaultHook(publicID, recordID); err != nil {
			return nil, false, fmt.Errorf("store: gate evaluation write fault: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("store: commit gate evaluation: %w", err)
	}
	committed = true

	evaluation, err = s.GetGateEvaluation(publicID)
	if err != nil {
		return nil, false, err
	}
	return evaluation, true, nil
}

// GetGateEvaluation returns the snapshot for one release record's public id,
// or (nil, nil) when the record or its snapshot does not exist.
func (s *Store) GetGateEvaluation(publicID string) (*GateEvaluation, error) {
	row := s.db.QueryRow(
		`SELECT e.id, r.id, r.public_id, r.environment, r.version, r.batch_id,
		        r.gate_status, e.effective_gate_status, r.recorded_at
		 FROM release_gate_evaluations e
		 JOIN release_records r ON r.id = e.record_id
		 WHERE r.public_id = ?`,
		publicID,
	)
	var evaluation GateEvaluation
	var evaluationID int64
	if err := row.Scan(
		&evaluationID, &evaluation.RecordID, &evaluation.PublicID,
		&evaluation.Environment, &evaluation.Version, &evaluation.BatchID,
		&evaluation.GateStatus, &evaluation.EffectiveGateStatus, &evaluation.RecordedAt,
	); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("store: get gate evaluation: %w", err)
	}
	checks, err := s.loadGateChecks(evaluationID)
	if err != nil {
		return nil, err
	}
	evaluation.Checks = checks
	return &evaluation, nil
}

// ListGateEvaluationsPage returns one keyset page of snapshots matching the
// filter, ordered by release newest first (recorded_at desc, internal record
// id desc). The anchor marks the last snapshot already returned; zero values
// request the first page. Up to limit+1 rows are fetched.
func (s *Store) ListGateEvaluationsPage(
	filter GateEvaluationFilter,
	anchorRecordedAt string,
	anchorID int64,
	limit int,
) ([]GateEvaluation, error) {
	where := []string{}
	args := []any{}
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
	if filter.CheckStatus != "" {
		where = append(where,
			`EXISTS (SELECT 1 FROM release_gate_checks c
			         WHERE c.evaluation_id = ge.id AND c.check_status = ?)`)
		args = append(args, filter.CheckStatus)
	}
	if anchorID > 0 {
		where = append(where, `(r.recorded_at < ? OR (r.recorded_at = ? AND r.id < ?))`)
		args = append(args, anchorRecordedAt, anchorRecordedAt, anchorID)
	}
	query := `SELECT r.id, r.public_id, r.environment, r.version, r.batch_id,
	                 r.gate_status, ge.effective_gate_status, r.recorded_at, ge.id
	          FROM (
	            SELECT r.id AS record_id, r.recorded_at
	            FROM release_gate_evaluations ge
	            JOIN release_records r ON r.id = ge.record_id`
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, ` AND `)
	}
	query += ` ORDER BY r.recorded_at DESC, r.id DESC LIMIT ?
	          ) AS page
	          JOIN release_records r ON r.id = page.record_id
	          JOIN release_gate_evaluations ge ON ge.record_id = r.id
	          ORDER BY page.recorded_at DESC, r.id DESC`
	args = append(args, limit+1)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list gate evaluations: %w", err)
	}
	defer rows.Close()

	evaluations := []GateEvaluation{}
	evaluationIDs := make([]int64, 0, limit+1)
	for rows.Next() {
		var evaluation GateEvaluation
		var evaluationID int64
		if err := rows.Scan(
			&evaluation.RecordID, &evaluation.PublicID, &evaluation.Environment,
			&evaluation.Version, &evaluation.BatchID, &evaluation.GateStatus,
			&evaluation.EffectiveGateStatus, &evaluation.RecordedAt, &evaluationID,
		); err != nil {
			return nil, fmt.Errorf("store: scan gate evaluation: %w", err)
		}
		evaluations = append(evaluations, evaluation)
		evaluationIDs = append(evaluationIDs, evaluationID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list gate evaluations: %w", err)
	}
	checksByEvaluation, err := s.loadGateChecksBatch(evaluationIDs)
	if err != nil {
		return nil, err
	}
	for i := range evaluations {
		evaluations[i].Checks = checksByEvaluation[evaluationIDs[i]]
	}
	return evaluations, nil
}

func loadGateChecksTx(tx *sql.Tx, evaluationID int64) ([]GateCheck, error) {
	rows, err := tx.Query(
		`SELECT check_name, check_status, evidence, waiver_reason
		 FROM release_gate_checks WHERE evaluation_id = ?
		 ORDER BY check_name ASC, id ASC`,
		evaluationID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: query gate checks: %w", err)
	}
	defer rows.Close()
	return scanGateChecks(rows)
}

func (s *Store) loadGateChecks(evaluationID int64) ([]GateCheck, error) {
	rows, err := s.db.Query(
		`SELECT check_name, check_status, evidence, waiver_reason
		 FROM release_gate_checks WHERE evaluation_id = ?
		 ORDER BY check_name ASC, id ASC`,
		evaluationID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: query gate checks: %w", err)
	}
	defer rows.Close()
	return scanGateChecks(rows)
}

func (s *Store) loadGateChecksBatch(evaluationIDs []int64) (map[int64][]GateCheck, error) {
	result := make(map[int64][]GateCheck, len(evaluationIDs))
	if len(evaluationIDs) == 0 {
		return result, nil
	}
	placeholders := strings.Repeat("?,", len(evaluationIDs))
	placeholders = placeholders[:len(placeholders)-1]
	rows, err := s.db.Query(
		`SELECT evaluation_id, check_name, check_status, evidence, waiver_reason
		 FROM release_gate_checks WHERE evaluation_id IN (`+placeholders+`)
		 ORDER BY check_name ASC, id ASC`,
		anySlice(evaluationIDs)...,
	)
	if err != nil {
		return nil, fmt.Errorf("store: query gate checks batch: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var evaluationID int64
		var check GateCheck
		if err := rows.Scan(
			&evaluationID, &check.Name, &check.Status, &check.Evidence, &check.WaiverReason,
		); err != nil {
			return nil, fmt.Errorf("store: scan gate check: %w", err)
		}
		result[evaluationID] = append(result[evaluationID], check)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: query gate checks batch: %w", err)
	}
	return result, nil
}

func scanGateChecks(rows *sql.Rows) ([]GateCheck, error) {
	checks := []GateCheck{}
	for rows.Next() {
		var check GateCheck
		if err := rows.Scan(
			&check.Name, &check.Status, &check.Evidence, &check.WaiverReason,
		); err != nil {
			return nil, fmt.Errorf("store: scan gate check: %w", err)
		}
		checks = append(checks, check)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: query gate checks: %w", err)
	}
	return checks, nil
}

func anySlice(ids []int64) []any {
	values := make([]any, len(ids))
	for i, id := range ids {
		values[i] = id
	}
	return values
}

// gateChecksEqual compares two slices already ordered by check name.
func gateChecksEqual(a, b []GateCheck) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
