package v1

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/luwa07832/deployment-tracker/internal/store"
)

var validGateStatuses = map[string]bool{"allowed": true, "blocked": true, "pending": true}

type changeEntryInput struct {
	Sequence    *int    `json:"sequence"`
	Category    *string `json:"category"`
	Title       *string `json:"title"`
	Description *string `json:"description"`
}

type createRecordInput struct {
	Environment   *string             `json:"environment"`
	Version       *string             `json:"version"`
	BatchID       *string             `json:"batch_id"`
	Changes       *[]changeEntryInput `json:"changes"`
	GateStatus    *string             `json:"gate_status"`
	RollbackPoint *string             `json:"rollback_point"`
}

type changeEntryView struct {
	Sequence    int    `json:"sequence"`
	Category    string `json:"category"`
	Title       string `json:"title"`
	Description string `json:"description"`
}

type recordView struct {
	ID            string            `json:"id"`
	Environment   string            `json:"environment"`
	Version       string            `json:"version"`
	BatchID       string            `json:"batch_id,omitempty"`
	GateStatus    string            `json:"gate_status"`
	RollbackPoint string            `json:"rollback_point"`
	Changes       []changeEntryView `json:"changes"`
	RecordedAt    string            `json:"recorded_at"`
}

func toRecordView(record *store.ReleaseRecord) recordView {
	view := recordView{
		ID:            record.PublicID,
		Environment:   record.Environment,
		Version:       record.Version,
		BatchID:       record.BatchID,
		GateStatus:    record.GateStatus,
		RollbackPoint: record.RollbackPoint,
		Changes:       make([]changeEntryView, 0, len(record.Changes)),
		RecordedAt:    record.RecordedAt,
	}
	for _, entry := range record.Changes {
		view.Changes = append(view.Changes, changeEntryView{
			Sequence:    entry.Sequence,
			Category:    entry.Category,
			Title:       entry.Title,
			Description: entry.Description,
		})
	}
	return view
}

func toRecordViews(records []store.ReleaseRecord) []recordView {
	views := make([]recordView, 0, len(records))
	for i := range records {
		views = append(views, toRecordView(&records[i]))
	}
	return views
}

// createReleaseRecord validates and stores one traceable release record.
func createReleaseRecord(deps Dependencies) gin.HandlerFunc {
	return func(c *gin.Context) {
		var input createRecordInput
		if err := json.NewDecoder(c.Request.Body).Decode(&input); err != nil {
			var typeErr *json.UnmarshalTypeError
			if errors.As(err, &typeErr) {
				fail(c, http.StatusUnprocessableEntity, store.CodeReleaseValidationV1,
					"field "+typeErr.Field+" has an invalid type")
				return
			}
			fail(c, http.StatusBadRequest, store.CodeInvalidRequest, "request body must be valid JSON")
			return
		}
		record, problem := validateRecordInput(&input)
		if problem != "" {
			fail(c, http.StatusUnprocessableEntity, store.CodeReleaseValidationV1, problem)
			return
		}
		if !requireEnvironment(c, deps, record.Environment) {
			return
		}
		if err := deps.Store.InsertReleaseRecord(record); err != nil {
			var duplicate *store.ErrReleaseAlreadyExists
			if errors.As(err, &duplicate) {
				fail(c, http.StatusConflict, store.CodeReleaseAlreadyExistsV1,
					"a release for "+record.Environment+" "+record.Version+" already exists")
				return
			}
			fail(c, http.StatusInternalServerError, store.CodeStorageUnavailable, "database is not available")
			return
		}
		c.JSON(http.StatusCreated, gin.H{"release_record": toRecordView(record)})
	}
}

// validateRecordInput applies the field rules and returns a ready-to-store
// record or a one-sentence problem. Sequence numbers are either supplied for
// every entry (unique positive integers) or for none (assigned by position).
func validateRecordInput(input *createRecordInput) (*store.ReleaseRecord, string) {
	if input.Environment == nil || strings.TrimSpace(*input.Environment) == "" {
		return nil, "environment is required"
	}
	if input.Version == nil || strings.TrimSpace(*input.Version) == "" {
		return nil, "version is required"
	}
	batchID := ""
	if input.BatchID != nil {
		batchID = strings.TrimSpace(*input.BatchID)
		if batchID == "" {
			return nil, "batch_id must not be blank when provided"
		}
		if len(batchID) > 200 {
			return nil, "batch_id must be at most 200 characters"
		}
	}
	if input.Changes == nil {
		return nil, "changes is required"
	}
	if input.GateStatus == nil || strings.TrimSpace(*input.GateStatus) == "" {
		return nil, "gate_status is required"
	}
	gateStatus := strings.TrimSpace(*input.GateStatus)
	if !validGateStatuses[gateStatus] {
		return nil, "gate_status must be one of: allowed, blocked, pending"
	}
	if input.RollbackPoint == nil || strings.TrimSpace(*input.RollbackPoint) == "" {
		return nil, "rollback_point is required"
	}
	rawEntries := *input.Changes
	entries := make([]store.ChangeEntry, 0, len(rawEntries))
	explicit := false
	seenSequences := map[int]bool{}
	for i, raw := range rawEntries {
		if raw.Category == nil || strings.TrimSpace(*raw.Category) == "" {
			return nil, "changes[" + strconv.Itoa(i) + "].category is required"
		}
		if raw.Title == nil || strings.TrimSpace(*raw.Title) == "" {
			return nil, "changes[" + strconv.Itoa(i) + "].title is required"
		}
		if raw.Description == nil || strings.TrimSpace(*raw.Description) == "" {
			return nil, "changes[" + strconv.Itoa(i) + "].description is required"
		}
		sequence := i + 1
		if raw.Sequence != nil {
			explicit = true
			if *raw.Sequence <= 0 {
				return nil, "changes[" + strconv.Itoa(i) + "].sequence must be a positive integer"
			}
			if seenSequences[*raw.Sequence] {
				return nil, "changes sequence numbers must be unique"
			}
			seenSequences[*raw.Sequence] = true
			sequence = *raw.Sequence
		}
		entries = append(entries, store.ChangeEntry{
			Sequence:    sequence,
			Category:    strings.TrimSpace(*raw.Category),
			Title:       strings.TrimSpace(*raw.Title),
			Description: strings.TrimSpace(*raw.Description),
		})
	}
	if explicit {
		for _, raw := range rawEntries {
			if raw.Sequence == nil {
				return nil, "changes sequence must be set for every entry or omitted for every entry"
			}
		}
	}
	return &store.ReleaseRecord{
		Environment:   strings.TrimSpace(*input.Environment),
		Version:       strings.TrimSpace(*input.Version),
		BatchID:       batchID,
		GateStatus:    gateStatus,
		RollbackPoint: strings.TrimSpace(*input.RollbackPoint),
		Changes:       entries,
	}, ""
}

// listReleaseRecords answers filtered queries ordered newest first.
func listReleaseRecords(deps Dependencies) gin.HandlerFunc {
	return func(c *gin.Context) {
		environment := strings.TrimSpace(c.Query("environment"))
		filter := store.ReleaseRecordFilter{
			Environment: environment,
			Version:     strings.TrimSpace(c.Query("version")),
			BatchID:     strings.TrimSpace(c.Query("batch_id")),
			GateStatus:  strings.TrimSpace(c.Query("gate_status")),
		}
		if environment != "" && !requireEnvironment(c, deps, environment) {
			return
		}
		if filter.GateStatus != "" && !validGateStatuses[filter.GateStatus] {
			fail(c, http.StatusUnprocessableEntity, store.CodeReleaseValidationV1,
				"gate_status must be one of: allowed, blocked, pending")
			return
		}
		if raw := strings.TrimSpace(c.Query("recorded_from")); raw != "" {
			bound, err := parseTimeBound(raw, false)
			if err != nil {
				fail(c, http.StatusUnprocessableEntity, store.CodeReleaseValidationV1,
					"recorded_from must be an RFC3339 timestamp or a YYYY-MM-DD date")
				return
			}
			filter.From = bound
		}
		if raw := strings.TrimSpace(c.Query("recorded_to")); raw != "" {
			bound, err := parseTimeBound(raw, true)
			if err != nil {
				fail(c, http.StatusUnprocessableEntity, store.CodeReleaseValidationV1,
					"recorded_to must be an RFC3339 timestamp or a YYYY-MM-DD date")
				return
			}
			filter.To = bound
		}
		records, err := deps.Store.ListReleaseRecords(filter)
		if err != nil {
			fail(c, http.StatusInternalServerError, store.CodeStorageUnavailable, "database is not available")
			return
		}
		c.JSON(http.StatusOK, gin.H{"release_records": toRecordViews(records)})
	}
}

// getReleaseRecord returns one record including its change entries and
// rollback point.
func getReleaseRecord(deps Dependencies) gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.Param("id")
		record, err := deps.Store.GetReleaseRecord(id)
		if err != nil {
			fail(c, http.StatusInternalServerError, store.CodeStorageUnavailable, "database is not available")
			return
		}
		if record == nil {
			fail(c, http.StatusNotFound, store.CodeReleaseRecordNotFoundV1, "no release record with id "+id)
			return
		}
		c.JSON(http.StatusOK, gin.H{"release_record": toRecordView(record)})
	}
}

// parseTimeBound accepts an RFC3339 instant or a calendar date. Dates expand
// to the start (00:00:00) or end (23:59:59) of that UTC day when the bound is
// inclusive.
func parseTimeBound(raw string, endOfDay bool) (string, error) {
	if instant, err := time.Parse(time.RFC3339, raw); err == nil {
		return instant.UTC().Format("2006-01-02T15:04:05Z"), nil
	}
	day, err := time.Parse("2006-01-02", raw)
	if err != nil {
		return "", err
	}
	if endOfDay {
		day = day.Add(24*time.Hour - time.Second)
	}
	return day.UTC().Format("2006-01-02T15:04:05Z"), nil
}
