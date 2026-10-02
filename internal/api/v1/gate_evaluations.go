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

// gateEvaluationCursorSigningKey signs opaque pagination cursors so a
// truncated, forged or condition-rebound cursor cannot be mistaken for a
// valid one. The key is process-private randomness; cursors never need to
// survive a restart.
var gateEvaluationCursorSigningKey = newGateEvaluationCursorSigningKey()

func newGateEvaluationCursorSigningKey() []byte {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic(err)
	}
	return key
}

type gateCheckInput struct {
	CheckName    *string `json:"check_name"`
	Status       *string `json:"status"`
	Evidence     *string `json:"evidence"`
	WaiverReason *string `json:"waiver_reason"`
}

type createGateEvaluationInput struct {
	Checks *[]gateCheckInput `json:"checks"`
}

type gateCheckView struct {
	CheckName    string `json:"check_name"`
	Status       string `json:"status"`
	Evidence     string `json:"evidence"`
	WaiverReason string `json:"waiver_reason,omitempty"`
}

type gateEvaluationView struct {
	ReleaseRecordID     string          `json:"release_record_id"`
	Environment         string          `json:"environment"`
	Version             string          `json:"version"`
	BatchID             string          `json:"batch_id,omitempty"`
	GateStatus          string          `json:"gate_status"`
	EffectiveGateStatus string          `json:"effective_gate_status"`
	Checks              []gateCheckView `json:"checks"`
}

func toGateEvaluationView(evaluation *store.GateEvaluation) gateEvaluationView {
	view := gateEvaluationView{
		ReleaseRecordID:     evaluation.PublicID,
		Environment:         evaluation.Environment,
		Version:             evaluation.Version,
		BatchID:             evaluation.BatchID,
		GateStatus:          evaluation.GateStatus,
		EffectiveGateStatus: evaluation.EffectiveGateStatus,
		Checks:              make([]gateCheckView, 0, len(evaluation.Checks)),
	}
	for _, check := range evaluation.Checks {
		view.Checks = append(view.Checks, gateCheckView{
			CheckName:    check.Name,
			Status:       check.Status,
			Evidence:     check.Evidence,
			WaiverReason: check.WaiverReason,
		})
	}
	return view
}

// createGateEvaluation accepts one immutable public gate-check snapshot for a
// release record. The derived decision must equal the record's stored
// gate_status; identical resubmissions are idempotent.
func createGateEvaluation(deps Dependencies) gin.HandlerFunc {
	return func(c *gin.Context) {
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
		checks, problem := validateGateChecks(input.Checks)
		if problem != "" {
			fail(c, http.StatusUnprocessableEntity, store.CodeGateEvaluationValidationV1, problem)
			return
		}

		evaluation, created, err := deps.Store.SaveGateEvaluation(c.Param("id"), checks)
		if err != nil {
			var missing *store.ErrReleaseRecordMissing
			if errors.As(err, &missing) {
				fail(c, http.StatusNotFound, store.CodeReleaseRecordNotFoundV1,
					"no release record with id "+missing.PublicID)
				return
			}
			var mismatch *store.ErrGateStatusMismatch
			if errors.As(err, &mismatch) {
				fail(c, http.StatusConflict, store.CodeGateStatusMismatchV1,
					"checks derive gate status "+mismatch.Derived+
						" but the release record stores "+mismatch.Recorded)
				return
			}
			var conflict *store.ErrGateEvaluationConflict
			if errors.As(err, &conflict) {
				fail(c, http.StatusConflict, store.CodeGateEvaluationConflictV1,
					"a gate evaluation with different checks already exists for "+conflict.PublicID)
				return
			}
			fail(c, http.StatusServiceUnavailable, store.CodeStorageUnavailable, "database is not available")
			return
		}
		status := http.StatusCreated
		if !created {
			status = http.StatusOK
		}
		c.JSON(status, gin.H{"gate_evaluation": toGateEvaluationView(evaluation)})
	}
}

// validateGateChecks applies the snapshot field rules and returns
// ready-to-store checks (not yet sorted; the store canonicalizes order).
func validateGateChecks(rawChecks *[]gateCheckInput) ([]store.GateCheck, string) {
	if rawChecks == nil {
		return nil, "checks is required"
	}
	if len(*rawChecks) == 0 {
		return nil, "checks must not be empty"
	}
	checks := make([]store.GateCheck, 0, len(*rawChecks))
	seenNames := map[string]bool{}
	for i, raw := range *rawChecks {
		position := "checks[" + strconv.Itoa(i) + "]"
		if raw.CheckName == nil || strings.TrimSpace(*raw.CheckName) == "" {
			return nil, position + ".check_name is required"
		}
		name := strings.TrimSpace(*raw.CheckName)
		if seenNames[name] {
			return nil, "check_name values must be unique: " + name
		}
		seenNames[name] = true
		if raw.Status == nil || strings.TrimSpace(*raw.Status) == "" {
			return nil, position + ".status is required"
		}
		status := strings.TrimSpace(*raw.Status)
		if !validGateCheckStatuses[status] {
			return nil, position + ".status must be one of: passed, failed, waived, pending"
		}
		if raw.Evidence == nil || strings.TrimSpace(*raw.Evidence) == "" {
			return nil, position + ".evidence is required"
		}
		waiverReason := ""
		if raw.WaiverReason != nil {
			waiverReason = strings.TrimSpace(*raw.WaiverReason)
		}
		if status == store.GateCheckWaived {
			if waiverReason == "" {
				return nil, position + ".waiver_reason is required when status is waived"
			}
		} else if raw.WaiverReason != nil {
			return nil, position + ".waiver_reason must be omitted when status is not waived"
		}
		checks = append(checks, store.GateCheck{
			Name:         name,
			Status:       status,
			Evidence:     strings.TrimSpace(*raw.Evidence),
			WaiverReason: waiverReason,
		})
	}
	return checks, ""
}

// getGateEvaluation returns one release record's gate snapshot with release
// locator, gate statuses and checks ordered by check_name.
func getGateEvaluation(deps Dependencies) gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.Param("id")
		evaluation, err := deps.Store.GetGateEvaluation(id)
		if err != nil {
			fail(c, http.StatusServiceUnavailable, store.CodeStorageUnavailable, "database is not available")
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
// cursor minted for the list endpoint.
type gateEvaluationCursorFilter struct {
	Environment string `json:"environment"`
	Version     string `json:"version"`
	GateStatus  string `json:"gate_status"`
	CheckStatus string `json:"check_status"`
}

// gateEvaluationCursor is the signed, opaque keyset marker. The position uses
// only the stable release-record public identifier, never internal keys.
type gateEvaluationCursor struct {
	Filter          gateEvaluationCursorFilter `json:"f"`
	ReleaseRecordID string                     `json:"id"`
}

func encodeGateEvaluationCursor(cursor gateEvaluationCursor) string {
	payload, _ := json.Marshal(cursor)
	mac := hmac.New(sha256.New, gateEvaluationCursorSigningKey)
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

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
	if cursor.Filter != expected || cursor.ReleaseRecordID == "" {
		return "", false
	}
	return cursor.ReleaseRecordID, true
}

// listGateEvaluations answers GET /api/v1/gate-evaluations: filtered,
// newest-first keyset pages over every stored snapshot.
func listGateEvaluations(deps Dependencies) gin.HandlerFunc {
	return func(c *gin.Context) {
		filter, cursorFilter, ok := parseGateEvaluationFilter(c)
		if !ok {
			return
		}

		limit := defaultGateEvaluationLimit
		if raw, present := c.GetQuery("limit"); present {
			raw = strings.TrimSpace(raw)
			if raw == "" {
				fail(c, http.StatusBadRequest, store.CodeInvalidGateEvaluationQueryV,
					"limit must not be blank when provided")
				return
			}
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed < 1 || parsed > maxGateEvaluationLimit {
				fail(c, http.StatusBadRequest, store.CodeInvalidGateEvaluationQueryV,
					"limit must be an integer between 1 and "+strconv.Itoa(maxGateEvaluationLimit))
				return
			}
			limit = parsed
		}
		if raw, present := c.GetQuery("cursor"); present && strings.TrimSpace(raw) == "" {
			fail(c, http.StatusBadRequest, store.CodeInvalidGateEvaluationQueryV,
				"cursor must not be blank when provided")
			return
		}

		var anchorRecordedAt string
		var anchorID int64
		if raw := strings.TrimSpace(c.Query("cursor")); raw != "" {
			publicID, valid := decodeGateEvaluationCursor(raw, cursorFilter)
			if !valid {
				fail(c, http.StatusBadRequest, store.CodeInvalidGateEvaluationQueryV,
					"cursor is not valid for this query")
				return
			}
			recordedAt, id, found, err := deps.Store.ResolveReleaseRecordPosition(publicID)
			if err != nil {
				fail(c, http.StatusServiceUnavailable, store.CodeStorageUnavailable,
					"database is not available")
				return
			}
			if !found {
				fail(c, http.StatusBadRequest, store.CodeInvalidGateEvaluationQueryV,
					"cursor is not valid for this query")
				return
			}
			anchorRecordedAt = recordedAt
			anchorID = id
		}

		if filter.Environment != "" && !requireEnvironment(c, deps, filter.Environment) {
			return
		}

		evaluations, err := deps.Store.ListGateEvaluationsPage(filter, anchorRecordedAt, anchorID, limit)
		if err != nil {
			fail(c, http.StatusServiceUnavailable, store.CodeStorageUnavailable, "database is not available")
			return
		}

		nextCursor := ""
		if len(evaluations) > limit {
			last := evaluations[limit-1]
			nextCursor = encodeGateEvaluationCursor(gateEvaluationCursor{
				Filter:          cursorFilter,
				ReleaseRecordID: last.PublicID,
			})
			evaluations = evaluations[:limit]
		}
		views := make([]gateEvaluationView, 0, len(evaluations))
		for i := range evaluations {
			views = append(views, toGateEvaluationView(&evaluations[i]))
		}
		c.JSON(http.StatusOK, gin.H{"gate_evaluations": views, "next_cursor": nextCursor})
	}
}

// parseGateEvaluationFilter validates every query condition before any
// storage access: blank parameters, enum values and limit shape.
func parseGateEvaluationFilter(c *gin.Context) (store.GateEvaluationFilter, gateEvaluationCursorFilter, bool) {
	get := func(key string) (string, bool) {
		raw, present := c.GetQuery(key)
		if present && strings.TrimSpace(raw) == "" {
			fail(c, http.StatusBadRequest, store.CodeInvalidGateEvaluationQueryV,
				key+" must not be blank when provided")
			return "", false
		}
		return strings.TrimSpace(raw), true
	}

	environment, ok := get("environment")
	if !ok {
		return store.GateEvaluationFilter{}, gateEvaluationCursorFilter{}, false
	}
	version, ok := get("version")
	if !ok {
		return store.GateEvaluationFilter{}, gateEvaluationCursorFilter{}, false
	}
	gateStatus, ok := get("gate_status")
	if !ok {
		return store.GateEvaluationFilter{}, gateEvaluationCursorFilter{}, false
	}
	if gateStatus != "" && !validGateStatuses[gateStatus] {
		fail(c, http.StatusBadRequest, store.CodeInvalidGateEvaluationQueryV,
			"gate_status must be one of: allowed, blocked, pending")
		return store.GateEvaluationFilter{}, gateEvaluationCursorFilter{}, false
	}
	checkStatus, ok := get("check_status")
	if !ok {
		return store.GateEvaluationFilter{}, gateEvaluationCursorFilter{}, false
	}
	if checkStatus != "" && !validGateCheckStatuses[checkStatus] {
		fail(c, http.StatusBadRequest, store.CodeInvalidGateEvaluationQueryV,
			"check_status must be one of: passed, failed, waived, pending")
		return store.GateEvaluationFilter{}, gateEvaluationCursorFilter{}, false
	}

	filter := store.GateEvaluationFilter{
		Environment: environment,
		Version:     version,
		GateStatus:  gateStatus,
		CheckStatus: checkStatus,
	}
	cursorFilter := gateEvaluationCursorFilter{
		Environment: environment,
		Version:     version,
		GateStatus:  gateStatus,
		CheckStatus: checkStatus,
	}
	return filter, cursorFilter, true
}
