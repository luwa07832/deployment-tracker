package store

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Gate check status values accepted in a submitted check snapshot.
const (
	GateCheckPassed  = "passed"
	GateCheckFailed  = "failed"
	GateCheckWaived  = "waived"
	GateCheckPending = "pending"
)

// Error codes introduced for the public gate evaluation API. Values are
// stable public identifiers returned verbatim in error responses.
const (
	CodeGateEvaluationNotFoundV1     = "GATE_EVALUATION_NOT_FOUND"
	CodeGateEvaluationConflictV1     = "GATE_EVALUATION_CONFLICT"
	CodeGateEvaluationValidationV1   = "GATE_EVALUATION_VALIDATION_FAILED"
	CodeGateStatusMismatchV1         = "GATE_STATUS_MISMATCH"
	CodeInvalidGateEvaluationQueryV1 = "INVALID_GATE_EVALUATION_QUERY"
)

// GateCheck is one immutable public gate check. WaiverReason stays empty for
// every status except waived.
type GateCheck struct {
	Position     int
	Name         string
	Status       string
	Evidence     string
	WaiverReason string
}

// GateEvaluation is the immutable check snapshot explaining one release
// record's gate status. GateStatus is derived from the checks and always
// equals the owning record's gate status; EffectiveGateStatus mirrors it so
// callers can read both facts from one snapshot.
type GateEvaluation struct {
	ID                  int64
	RecordID            int64
	RecordPublicID      string
	Environment         string
	Version             string
	BatchID             string
	GateStatus          string
	EffectiveGateStatus string
	CreatedAt           string
	Checks              []GateCheck
}

// GateEvaluationFilter narrows ListGateEvaluations. Zero-valued fields are
// ignored. CheckStatus matches evaluations containing at least one check with
// that status.
type GateEvaluationFilter struct {
	Environment string
	Version     string
	GateStatus  string
	CheckStatus string
}

// ErrGateEvaluationRecordNotFound marks a submission for a release record
// public id that does not exist.
var ErrGateEvaluationRecordNotFound = errors.New("store: no release record for gate evaluation")

// ErrGateEvaluationConflict marks a second submission for the same release
// record carrying different checks. The first snapshot stays authoritative.
type ErrGateEvaluationConflict struct{}

func (e *ErrGateEvaluationConflict) Error() string {
	return "gate evaluation already exists with different checks"
}

// ErrGateStatusMismatch marks checks whose derived gate status differs from
// the gate status already stored on the release record.
type ErrGateStatusMismatch struct {
	RecordStatus string
	CheckStatus  string
}

func (e *ErrGateStatusMismatch) Error() string {
	return "checks derive gate status " + e.CheckStatus +
		" but the release record declares " + e.RecordStatus
}

// DeriveGateStatus computes the effective gate status from checks: any failed
// check blocks, otherwise any pending check leaves the gate pending, and a
// snapshot with only passed or waived checks allows the release.
func DeriveGateStatus(checks []GateCheck) string {
	pending := false
	for _, check := range checks {
		switch check.Status {
		case GateCheckFailed:
			return "blocked"
		case GateCheckPending:
			pending = true
		}
	}
	if pending {
		return "pending"
	}
	return "allowed"
}

// SaveGateEvaluation atomically stores the immutable check snapshot for the
// release record identified by publicID. Repeating the same checks returns
// the stored snapshot with existing == true. Different checks on an existing
// snapshot return *ErrGateEvaluationConflict; a derived status that differs
// from the record's gate_status returns *ErrGateStatusMismatch. An unknown
// record id returns ErrGateEvaluationRecordNotFound.
func (s *Store) SaveGateEvaluation(publicID string, checks []GateCheck) (evaluation *GateEvaluation, existing bool, err error) {
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

	var recordPK int64
	var recordGateStatus string
	err = tx.QueryRow(
		`SELECT id, gate_status FROM release_records WHERE public_id = ?`, publicID,
	).Scan(&recordPK, &recordGateStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, ErrGateEvaluationRecordNotFound
	}
	if err != nil {
		return nil, false, fmt.Errorf("store: load release record for gate evaluation: %w", err)
	}

	derived := DeriveGateStatus(checks)
	if recordGateStatus != derived {
		return nil, false, &ErrGateStatusMismatch{
			RecordStatus: recordGateStatus,
			CheckStatus:  derived,
		}
	}

	var evaluationID int64
	err = tx.QueryRow(
		`SELECT id FROM release_gate_evaluations WHERE record_id = ?`, recordPK,
	).Scan(&evaluationID)
	switch {
	case err == nil:
		storedChecks, loadErr := loadGateChecksTx(tx, evaluationID)
		if loadErr != nil {
			return nil, false, loadErr
		}
		if !sameGateChecks(storedChecks, checks) {
			return nil, false, &ErrGateEvaluationConflict{}
		}
		existing = true
	case errors.Is(err, sql.ErrNoRows):
		res, insertErr := tx.Exec(
			`INSERT INTO release_gate_evaluations (record_id, gate_status) VALUES (?, ?)`,
			recordPK, derived,
		)
		if insertErr != nil {
			return nil, false, fmt.Errorf("store: insert gate evaluation: %w", insertErr)
		}
		evaluationID, insertErr = res.LastInsertId()
		if insertErr != nil {
			return nil, false, fmt.Errorf("store: insert gate evaluation: %w", insertErr)
		}
		for i := range checks {
			if _, insertErr := tx.Exec(
				`INSERT INTO release_gate_checks
				 (evaluation_id, position, check_name, status, evidence, waiver_reason)
				 VALUES (?, ?, ?, ?, ?, ?)`,
				evaluationID, i, checks[i].Name, checks[i].Status,
				checks[i].Evidence, checks[i].WaiverReason,
			); insertErr != nil {
				return nil, false, fmt.Errorf("store: insert gate check: %w", insertErr)
			}
		}
	default:
		return nil, false, fmt.Errorf("store: check gate evaluation: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("store: commit gate evaluation: %w", err)
	}
	committed = true

	snapshot, err := s.GetGateEvaluation(publicID)
	if err != nil {
		return nil, false, err
	}
	if snapshot == nil {
		return nil, false, fmt.Errorf("store: gate evaluation vanished after commit")
	}
	return snapshot, existing, nil
}

// loadGateChecksTx returns the stored checks ordered by check_name, matching
// the order snapshots are served in.
func loadGateChecksTx(tx *sql.Tx, evaluationID int64) ([]GateCheck, error) {
	rows, err := tx.Query(
		`SELECT position, check_name, status, evidence, waiver_reason
		 FROM release_gate_checks WHERE evaluation_id = ?
		 ORDER BY check_name ASC, id ASC`,
		evaluationID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: load gate checks: %w", err)
	}
	defer rows.Close()
	checks := []GateCheck{}
	for rows.Next() {
		var check GateCheck
		if err := rows.Scan(&check.Position, &check.Name, &check.Status,
			&check.Evidence, &check.WaiverReason); err != nil {
			return nil, fmt.Errorf("store: scan gate check: %w", err)
		}
		checks = append(checks, check)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: load gate checks: %w", err)
	}
	return checks, nil
}

// sameGateChecks compares snapshots by content, not submission order: checks
// are uniquely named and always served sorted by name, so two submissions
// with the same names in a different order describe the same snapshot.
func sameGateChecks(left, right []GateCheck) bool {
	if len(left) != len(right) {
		return false
	}
	a := append([]GateCheck(nil), left...)
	b := append([]GateCheck(nil), right...)
	sortChecksByName(a)
	sortChecksByName(b)
	for i := range a {
		if a[i].Name != b[i].Name ||
			a[i].Status != b[i].Status ||
			a[i].Evidence != b[i].Evidence ||
			a[i].WaiverReason != b[i].WaiverReason {
			return false
		}
	}
	return true
}

func sortChecksByName(checks []GateCheck) {
	sort.Slice(checks, func(i, j int) bool { return checks[i].Name < checks[j].Name })
}

// gateEvaluationSelectColumns lists the snapshot columns followed by the
// nullable check join columns in scan order.
const gateEvaluationSelectColumns = `ge.id, ge.gate_status, ge.created_at,
	r.public_id, r.environment, r.version, r.batch_id,
	c.id, c.position, c.check_name, c.status, c.evidence, c.waiver_reason`

// scanGateEvaluationRows groups evaluation/check join rows into snapshots with
// checks sorted by check_name. Rows must arrive ordered by evaluation id
// descending followed by check_name ascending.
func scanGateEvaluationRows(rows *sql.Rows) ([]GateEvaluation, error) {
	evaluations := []GateEvaluation{}
	indexByID := map[int64]int{}
	for rows.Next() {
		var evaluation GateEvaluation
		var checkID, checkPosition sql.NullInt64
		var checkName, checkStatus, checkEvidence, checkWaiver sql.NullString
		if err := rows.Scan(
			&evaluation.ID, &evaluation.GateStatus, &evaluation.CreatedAt,
			&evaluation.RecordPublicID, &evaluation.Environment, &evaluation.Version,
			&evaluation.BatchID,
			&checkID, &checkPosition, &checkName, &checkStatus,
			&checkEvidence, &checkWaiver,
		); err != nil {
			return nil, fmt.Errorf("store: scan gate evaluation: %w", err)
		}
		evaluation.EffectiveGateStatus = evaluation.GateStatus
		pos, seen := indexByID[evaluation.ID]
		if !seen {
			evaluation.Checks = []GateCheck{}
			evaluations = append(evaluations, evaluation)
			pos = len(evaluations) - 1
			indexByID[evaluation.ID] = pos
		}
		if checkName.Valid {
			evaluations[pos].Checks = append(evaluations[pos].Checks, GateCheck{
				Position:     int(checkPosition.Int64),
				Name:         checkName.String,
				Status:       checkStatus.String,
				Evidence:     checkEvidence.String,
				WaiverReason: checkWaiver.String,
			})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: query gate evaluations: %w", err)
	}
	return evaluations, nil
}

// GetGateEvaluation returns the snapshot attached to one release record, or
// (nil, nil) when the record or its snapshot does not exist.
func (s *Store) GetGateEvaluation(publicID string) (*GateEvaluation, error) {
	rows, err := s.db.Query(
		`SELECT `+gateEvaluationSelectColumns+`
		 FROM release_gate_evaluations ge
		 JOIN release_records r ON r.id = ge.record_id
		 LEFT JOIN release_gate_checks c ON c.evaluation_id = ge.id
		 WHERE r.public_id = ?
		 ORDER BY ge.id DESC, c.check_name ASC, c.id ASC`,
		publicID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: query gate evaluation: %w", err)
	}
	defer rows.Close()
	evaluations, err := scanGateEvaluationRows(rows)
	if err != nil {
		return nil, err
	}
	if len(evaluations) == 0 {
		return nil, nil
	}
	return &evaluations[0], nil
}

// ListGateEvaluationsPage returns one keyset page of snapshots matching the
// filter, newest evaluation first (internal id descending). The anchor marks
// the last evaluation already returned; zero requests the first page. Up to
// limit+1 rows are fetched so the caller can detect a following page.
func (s *Store) ListGateEvaluationsPage(
	filter GateEvaluationFilter,
	anchorID int64,
	limit int,
) ([]GateEvaluation, error) {
	where := []string{}
	args := []any{}
	if filter.Environment != "" {
		where = append(where, `r0.environment = ?`)
		args = append(args, filter.Environment)
	}
	if filter.Version != "" {
		where = append(where, `r0.version = ?`)
		args = append(args, filter.Version)
	}
	if filter.GateStatus != "" {
		where = append(where, `ge.gate_status = ?`)
		args = append(args, filter.GateStatus)
	}
	if filter.CheckStatus != "" {
		where = append(where,
			`EXISTS (SELECT 1 FROM release_gate_checks gc
			         WHERE gc.evaluation_id = ge.id AND gc.status = ?)`)
		args = append(args, filter.CheckStatus)
	}
	if anchorID > 0 {
		where = append(where, `ge.id < ?`)
		args = append(args, anchorID)
	}
	query := `SELECT page.id, page.gate_status, page.created_at,
		          r.public_id, r.environment, r.version, r.batch_id,
		          c.id, c.position, c.check_name, c.status, c.evidence, c.waiver_reason
		 FROM (
		   SELECT ge.id AS id, ge.record_id AS record_id, ge.gate_status AS gate_status,
		          ge.created_at AS created_at
		   FROM release_gate_evaluations ge
		   JOIN release_records r0 ON r0.id = ge.record_id`
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, ` AND `)
	}
	query += ` ORDER BY ge.id DESC LIMIT ?
		 ) AS page
		 JOIN release_records r ON r.id = page.record_id
		 LEFT JOIN release_gate_checks c ON c.evaluation_id = page.id
		 ORDER BY page.id DESC, c.check_name ASC, c.id ASC`
	args = append(args, limit+1)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query gate evaluations: %w", err)
	}
	defer rows.Close()
	return scanGateEvaluationRows(rows)
}

// ResolveGateEvaluationPosition maps the owning release record's stable
// public identifier to the internal keyset position of its evaluation. It
// returns ok == false when no snapshot exists.
func (s *Store) ResolveGateEvaluationPosition(publicID string) (id int64, ok bool, err error) {
	err = s.db.QueryRow(
		`SELECT ge.id FROM release_gate_evaluations ge
		 JOIN release_records r ON r.id = ge.record_id
		 WHERE r.public_id = ?`,
		publicID,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("store: resolve gate evaluation position: %w", err)
	}
	return id, true, nil
}

// CountGateEvaluationsForTest returns the total number of stored snapshots.
// Test-only.
func (s *Store) CountGateEvaluationsForTest(count *int) error {
	return s.db.QueryRow(`SELECT count(*) FROM release_gate_evaluations`).Scan(count)
}
