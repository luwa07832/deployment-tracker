package v1

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/luwa07832/deployment-tracker/internal/store"
)

const (
	defaultGateEvaluationLimit = 20
	maxGateEvaluationLimit     = 100
)

var validGateCheckStatuses = map[string]bool{
	store.GateCheckPassed:  true,
	store.GateCheckFailed:  true,
	store.GateCheckWaived:  true,
	store.GateCheckPending: true,
}

// gateEvaluationCursorSigningKey signs opaque list cursors so a truncated,
// forged or filter-rebound cursor cannot be mistaken for a valid one.
var gateEvaluationCursorSigningKey = newGateEvaluationCursorSigningKey()

func newGateEvaluationCursorSigningKey() []byte {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic(err)
	}
	return key
}

type gateCheckInput struct {
	Name         *string `json:"check_name"`
	Status       *string `json:"status"`
	Evidence     *string `json:"evidence"`
	WaiverReason *string `json:"waiver_reason"`
}

type createGateEvaluationInput struct {
	Checks *[]gateCheckInput `json:"checks"`
}

type gateCheckView struct {
	Name         string `json:"check_name"`
	Status       string `json:"status"`
	Evidence     string `json:"evidence"`
	WaiverReason string `json:"waiver_reason,omitempty"`
}

type gateEvaluationView struct {
	Release             string          `json:"release"`
	Environment         string          `json:"environment"`
	Version             string          `json:"version"`
	BatchID             string          `json:"batch_id,omitempty"`
	GateStatus          string          `json:"gate_status"`
	EffectiveGateStatus string          `json:"effective_gate_status"`
	Checks              []gateCheckView `json:"checks"`
}

func toGateEvaluationView(evaluation *store.GateEvaluation) gateEvaluationView {
	view := gateEvaluationView{
		Release:             evaluation.RecordPublicID,
		Environment:         evaluation.Environment,
		Version:             evaluation.Version,
		BatchID:             evaluation.BatchID,
		GateStatus:          evaluation.GateStatus,
		EffectiveGateStatus: evaluation.EffectiveGateStatus,
		Checks:              make([]gateCheckView, 0, len(evaluation.Checks)),
	}
	for _, check := range evaluation.Checks {
		item := gateCheckView{
			Name:     check.Name,
			Status:   check.Status,
			Evidence: check.Evidence,
		}
		if check.Status == store.GateCheckWaived {
			item.WaiverReason = check.WaiverReason
		}
		view.Checks = append(view.Checks, item)
	}
	return view
}

// validateGateChecks applies the field rules and returns the normalized checks
// in submission order, or a one-sentence problem. Duplicate names are rejected
// regardless of trimming; returned checks are trimmed.
func validateGateChecks(rawChecks []gateCheckInput) ([]store.GateCheck, string) {
	checks := make([]store.GateCheck, 0, len(rawChecks))
	seenNames := map[string]bool{}
	for i, raw := range rawChecks {
		prefix := "checks[" + strconv.Itoa(i) + "]"
		if raw.Name == nil || strings.TrimSpace(*raw.Name) == "" {
			return nil, prefix + ".check_name is required"
		}
		name := strings.TrimSpace(*raw.Name)
		if seenNames[name] {
			return nil, "checks check_name values must be unique"
		}
		seenNames[name] = true
		if raw.Status == nil || strings.TrimSpace(*raw.Status) == "" {
			return nil, prefix + ".status is required"
		}
		status := strings.TrimSpace(*raw.Status)
		if !validGateCheckStatuses[status] {
			return nil, prefix + ".status must be one of: passed, failed, waived, pending"
		}
		if raw.Evidence == nil || strings.TrimSpace(*raw.Evidence) == "" {
			return nil, prefix + ".evidence is required"
		}
		evidence := strings.TrimSpace(*raw.Evidence)
		waiverReason := ""
		if status == store.GateCheckWaived {
			if raw.WaiverReason == nil || strings.TrimSpace(*raw.WaiverReason) == "" {
				return nil, prefix + ".waiver_reason is required when status is waived"
			}
			waiverReason = strings.TrimSpace(*raw.WaiverReason)
		} else if raw.WaiverReason != nil {
			return nil, prefix + ".waiver_reason must be omitted unless status is waived"
		}
		checks = append(checks, store.GateCheck{
			Position:     i + 1,
			Name:         name,
			Status:       status,
			Evidence:     evidence,
			WaiverReason: waiverReason,
		})
	}
	return checks, ""
}

// createGateEvaluation accepts an immutable check snapshot for a release
// record. Repeating the same checks returns the stored snapshot with 200; a
// second, different snapshot is rejected with 409.
func createGateEvaluation(deps Dependencies) gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.Param("id")
		var input createGateEvaluationInput
		if err := json.NewDecoder(c.Request.Body).Decode(&input); err != nil {
			var typeErr *json.UnmarshalTypeError
			if errors.As(err, &typeErr) {
				fail(c, http.StatusUnprocessableEntity, store.CodeGateEvaluationValidationV1,
					"field "+typeErr.Field+" has an invalid type")
				return
			}
			fail(c, http.StatusBadRequest, store.CodeInvalidRequest, "request body must be valid JSON")
			return
		}
		if input.Checks == nil {
			fail(c, http.StatusUnprocessableEntity, store.CodeGateEvaluationValidationV1,
				"checks is required")
			return
		}
		rawChecks := *input.Checks
		if len(rawChecks) == 0 {
			fail(c, http.StatusUnprocessableEntity, store.CodeGateEvaluationValidationV1,
				"checks must not be empty")
			return
		}
		checks, problem := validateGateChecks(rawChecks)
		if problem != "" {
			fail(c, http.StatusUnprocessableEntity, store.CodeGateEvaluationValidationV1, problem)
			return
		}
		evaluation, _, err := deps.Store.SaveGateEvaluation(id, checks)
		switch {
		case errors.Is(err, store.ErrGateEvaluationRecordNotFound):
			fail(c, http.StatusNotFound, store.CodeReleaseRecordNotFoundV1,
				"no release record with id "+id)
			return
		case isGateEvaluationConflict(err):
			fail(c, http.StatusConflict, store.CodeGateEvaluationConflictV1,
				"a gate evaluation with different checks already exists for this release record")
			return
		case isGateStatusMismatch(err):
			mismatch := err.(*store.ErrGateStatusMismatch)
			fail(c, http.StatusConflict, store.CodeGateStatusMismatchV1,
				"checks derive gate status "+mismatch.CheckStatus+
					" but the release record declares "+mismatch.RecordStatus)
			return
		case err != nil:
			fail(c, http.StatusServiceUnavailable, store.CodeStorageUnavailable,
				"database is not available")
			return
		}
		c.JSON(http.StatusOK, gin.H{"gate_evaluation": toGateEvaluationView(evaluation)})
	}
}

func isGateEvaluationConflict(err error) bool {
	var conflict *store.ErrGateEvaluationConflict
	return errors.As(err, &conflict)
}

func isGateStatusMismatch(err error) bool {
	var mismatch *store.ErrGateStatusMismatch
	return errors.As(err, &mismatch)
}

// getGateEvaluation returns the single immutable snapshot attached to a
// release record. A missing record and a record without a snapshot both
// answer 404 GATE_EVALUATION_NOT_FOUND here.
func getGateEvaluation(deps Dependencies) gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.Param("id")
		evaluation, err := deps.Store.GetGateEvaluation(id)
		if err != nil {
			fail(c, http.StatusServiceUnavailable, store.CodeStorageUnavailable,
				"database is not available")
			return
		}
		if evaluation == nil {
			fail(c, http.StatusNotFound, store.CodeGateEvaluationNotFoundV1,
				"no gate evaluation for release record "+id)
			return
		}
		c.JSON(http.StatusOK, gin.H{"gate_evaluation": toGateEvaluationView(evaluation)})
	}
}

// gateEvaluationCursorFilter is the normalized query snapshot bound to every
// cursor the list endpoint mints.
type gateEvaluationCursorFilter struct {
	Environment string `json:"environment"`
	Version     string `json:"version"`
	GateStatus  string `json:"gate_status"`
	CheckStatus string `json:"check_status"`
}

// gateEvaluationCursor is the signed, opaque keyset marker. The position
// carries only the owning record's stable public identifier, never the
// internal database key.
type gateEvaluationCursor struct {
	Filter  gateEvaluationCursorFilter `json:"f"`
	Release string                     `json:"id"`
}

func encodeGateEvaluationCursor(cursor gateEvaluationCursor) string {
	payload, _ := json.Marshal(cursor)
	mac := hmac.New(sha256.New, gateEvaluationCursorSigningKey)
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// decodeGateEvaluationCursor validates shape, signature and filter binding.
// Any tampering, truncation or condition mismatch yields false.
func decodeGateEvaluationCursor(raw string, expected gateEvaluationCursorFilter) (string, bool) {
	parts := strings.Split(strings.TrimSpace(raw), ".")
	if len(parts) != 2 {
		return "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", false
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false
	}
	mac := hmac.New(sha256.New, gateEvaluationCursorSigningKey)
	mac.Write(payload)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return "", false
	}
	var cursor gateEvaluationCursor
	if err := json.Unmarshal(payload, &cursor); err != nil {
		return "", false
	}
	if cursor.Filter != expected || cursor.Release == "" {
		return "", false
	}
	return cursor.Release, true
}

// listGateEvaluations returns a newest-first, keyset-paginated page of
// snapshots across release records, filtered by environment, version,
// gate_status and check status.
func listGateEvaluations(deps Dependencies) gin.HandlerFunc {
	return func(c *gin.Context) {
		trimmed := func(key string) (string, bool) {
			raw, present := c.GetQuery(key)
			if present && strings.TrimSpace(raw) == "" {
				fail(c, http.StatusBadRequest, store.CodeInvalidGateEvaluationQueryV1,
					key+" must not be blank when provided")
				return "", false
			}
			return strings.TrimSpace(raw), true
		}
		environment, ok := trimmed("environment")
		if !ok {
			return
		}
		version, ok := trimmed("version")
		if !ok {
			return
		}
		gateStatus, ok := trimmed("gate_status")
		if !ok {
			return
		}
		if gateStatus != "" && !validGateStatuses[gateStatus] {
			fail(c, http.StatusBadRequest, store.CodeInvalidGateEvaluationQueryV1,
				"gate_status must be one of: allowed, blocked, pending")
			return
		}
		checkStatus, ok := trimmed("check_status")
		if !ok {
			return
		}
		if checkStatus != "" && !validGateCheckStatuses[checkStatus] {
			fail(c, http.StatusBadRequest, store.CodeInvalidGateEvaluationQueryV1,
				"check_status must be one of: passed, failed, waived, pending")
			return
		}
		filter := store.GateEvaluationFilter{
			Environment: environment,
			Version:     version,
			GateStatus:  gateStatus,
			CheckStatus: checkStatus,
		}

		limit := defaultGateEvaluationLimit
		if raw, present := c.GetQuery("limit"); present {
			raw = strings.TrimSpace(raw)
			if raw == "" {
				fail(c, http.StatusBadRequest, store.CodeInvalidGateEvaluationQueryV1,
					"limit must not be blank when provided")
				return
			}
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed < 1 || parsed > maxGateEvaluationLimit {
				fail(c, http.StatusBadRequest, store.CodeInvalidGateEvaluationQueryV1,
					"limit must be an integer between 1 and "+strconv.Itoa(maxGateEvaluationLimit))
				return
			}
			limit = parsed
		}

		cursorFilter := gateEvaluationCursorFilter{
			Environment: filter.Environment,
			Version:     filter.Version,
			GateStatus:  filter.GateStatus,
			CheckStatus: filter.CheckStatus,
		}
		var anchorID int64
		if raw, present := c.GetQuery("cursor"); present {
			raw = strings.TrimSpace(raw)
			if raw == "" {
				fail(c, http.StatusBadRequest, store.CodeInvalidGateEvaluationQueryV1,
					"cursor must not be blank when provided")
				return
			}
			publicID, valid := decodeGateEvaluationCursor(raw, cursorFilter)
			if !valid {
				fail(c, http.StatusBadRequest, store.CodeInvalidGateEvaluationQueryV1,
					"cursor is not valid for this query")
				return
			}
			resolvedID, valid, err := deps.Store.ResolveGateEvaluationPosition(publicID)
			if err != nil {
				fail(c, http.StatusServiceUnavailable, store.CodeStorageUnavailable,
					"database is not available")
				return
			}
			if !valid {
				fail(c, http.StatusBadRequest, store.CodeInvalidGateEvaluationQueryV1,
					"cursor is not valid for this query")
				return
			}
			anchorID = resolvedID
		}

		if filter.Environment != "" && !requireEnvironment(c, deps, filter.Environment) {
			return
		}

		evaluations, err := deps.Store.ListGateEvaluationsPage(filter, anchorID, limit)
		if err != nil {
			fail(c, http.StatusServiceUnavailable, store.CodeStorageUnavailable,
				"database is not available")
			return
		}

		nextCursor := ""
		if len(evaluations) > limit {
			last := evaluations[limit-1]
			nextCursor = encodeGateEvaluationCursor(gateEvaluationCursor{
				Filter:  cursorFilter,
				Release: last.RecordPublicID,
			})
			evaluations = evaluations[:limit]
		}
		views := make([]gateEvaluationView, 0, len(evaluations))
		for i := range evaluations {
			views = append(views, toGateEvaluationView(&evaluations[i]))
		}
		c.JSON(http.StatusOK, gin.H{
			"gate_evaluations": views,
			"next_cursor":      nextCursor,
		})
	}
}
