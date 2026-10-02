package v1

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/luwa07832/deployment-tracker/internal/store"
)

const (
	defaultChangeEntryLimit = 20
	maxChangeEntryLimit     = 100
)

var errNonCanonicalTime = errors.New("time is not in the canonical format")

// changeEntryCursor is the opaque keyset marker. It carries the bound filter
// along with the internal position of the last row returned, so a cursor is
// only accepted with the exact query it was minted for.
type changeEntryCursor struct {
	RecordedAt string `json:"recorded_at"`
	RecordID   int64  `json:"record_id"`
	Sequence   int    `json:"sequence"`
	Title      string `json:"title"`
	EntryID    int64  `json:"entry_id"`

	Environment string `json:"environment"`
	Version     string `json:"version"`
	BatchID     string `json:"batch_id"`
	Category    string `json:"category"`
	EntryFilter string `json:"entry_title"`
	GateStatus  string `json:"gate_status"`
	From        string `json:"from"`
	To          string `json:"to"`
}

func encodeChangeEntryCursor(cursor changeEntryCursor) string {
	raw, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(raw)
}

// decodeChangeEntryCursor parses an opaque marker and verifies that it is
// structurally complete. Filter binding is checked separately.
func decodeChangeEntryCursor(raw string) (changeEntryCursor, bool) {
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return changeEntryCursor{}, false
	}
	var cursor changeEntryCursor
	if err := json.Unmarshal(decoded, &cursor); err != nil {
		return changeEntryCursor{}, false
	}
	if cursor.RecordedAt == "" || cursor.RecordID <= 0 ||
		cursor.Sequence <= 0 || cursor.Title == "" || cursor.EntryID <= 0 {
		return changeEntryCursor{}, false
	}
	if _, err := parseChangeEntryTime(cursor.RecordedAt); err != nil {
		return changeEntryCursor{}, false
	}
	return cursor, true
}

type changeEntryItemView struct {
	Sequence      int    `json:"sequence"`
	Category      string `json:"category"`
	Title         string `json:"title"`
	Description   string `json:"description"`
	ID            string `json:"id"`
	Environment   string `json:"environment"`
	Version       string `json:"version"`
	BatchID       string `json:"batch_id,omitempty"`
	GateStatus    string `json:"gate_status"`
	RollbackPoint string `json:"rollback_point"`
	RecordedAt    string `json:"recorded_at"`
}

type changeEntriesPageView struct {
	Changes    []changeEntryItemView `json:"changes"`
	NextCursor string                `json:"next_cursor"`
}

// changeQueryParamNames lists every parameter this entry point understands.
// A present-but-blank value is always a client error.
var changeQueryParamNames = []string{
	"environment", "version", "batch_id", "category", "title", "gate_status",
	"from", "to", "limit", "cursor",
}

func failChangeQuery(c *gin.Context, message string) {
	fail(c, http.StatusBadRequest, store.CodeInvalidChangeQueryV1, message)
}

// parseChangeEntryQuery validates the request before any storage access and
// returns the normalized filter, page size and keyset anchor.
func parseChangeEntryQuery(c *gin.Context) (store.ChangeEntryFilter, int, store.ChangeEntryAnchor, bool) {
	query := c.Request.URL.Query()
	values := map[string]string{}
	for _, name := range changeQueryParamNames {
		rawValues, present := query[name]
		if !present {
			continue
		}
		raw := rawValues[0]
		if strings.TrimSpace(raw) == "" {
			failChangeQuery(c, name+" must not be blank")
			return store.ChangeEntryFilter{}, 0, store.ChangeEntryAnchor{}, false
		}
		values[name] = strings.TrimSpace(raw)
	}

	filter := store.ChangeEntryFilter{
		Environment: values["environment"],
		Version:     values["version"],
		BatchID:     values["batch_id"],
		Category:    values["category"],
		Title:       values["title"],
		GateStatus:  values["gate_status"],
	}
	if filter.GateStatus != "" && !validGateStatuses[filter.GateStatus] {
		failChangeQuery(c, "gate_status must be one of: allowed, blocked, pending")
		return store.ChangeEntryFilter{}, 0, store.ChangeEntryAnchor{}, false
	}
	if raw := values["from"]; raw != "" {
		from, err := parseChangeEntryTime(raw)
		if err != nil {
			failChangeQuery(c, "from must be a UTC second-precision timestamp in YYYY-MM-DDTHH:MM:SSZ format")
			return store.ChangeEntryFilter{}, 0, store.ChangeEntryAnchor{}, false
		}
		filter.From = from
	}
	if raw := values["to"]; raw != "" {
		to, err := parseChangeEntryTime(raw)
		if err != nil {
			failChangeQuery(c, "to must be a UTC second-precision timestamp in YYYY-MM-DDTHH:MM:SSZ format")
			return store.ChangeEntryFilter{}, 0, store.ChangeEntryAnchor{}, false
		}
		filter.To = to
	}
	if filter.From != "" && filter.To != "" && filter.From > filter.To {
		failChangeQuery(c, "from must not be later than to")
		return store.ChangeEntryFilter{}, 0, store.ChangeEntryAnchor{}, false
	}

	limit := defaultChangeEntryLimit
	if raw := values["limit"]; raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > maxChangeEntryLimit {
			failChangeQuery(c, "limit must be an integer between 1 and 100")
			return store.ChangeEntryFilter{}, 0, store.ChangeEntryAnchor{}, false
		}
		limit = parsed
	}

	var anchor store.ChangeEntryAnchor
	if raw := values["cursor"]; raw != "" {
		cursor, ok := decodeChangeEntryCursor(raw)
		if !ok {
			failChangeQuery(c, "cursor is not a valid pagination cursor")
			return store.ChangeEntryFilter{}, 0, store.ChangeEntryAnchor{}, false
		}
		if cursor.Environment != filter.Environment || cursor.Version != filter.Version ||
			cursor.BatchID != filter.BatchID || cursor.Category != filter.Category ||
			cursor.EntryFilter != filter.Title || cursor.GateStatus != filter.GateStatus ||
			cursor.From != filter.From || cursor.To != filter.To {
			failChangeQuery(c, "cursor does not match the query parameters")
			return store.ChangeEntryFilter{}, 0, store.ChangeEntryAnchor{}, false
		}
		anchor = store.ChangeEntryAnchor{
			RecordedAt: cursor.RecordedAt,
			RecordID:   cursor.RecordID,
			Sequence:   cursor.Sequence,
			Title:      cursor.Title,
			EntryID:    cursor.EntryID,
		}
	}
	return filter, limit, anchor, true
}

// listChangeEntries answers structured-change queries over v1 release
// records only. Baseline POST /releases changes live in a different table
// and are never part of the result.
func listChangeEntries(deps Dependencies) gin.HandlerFunc {
	return func(c *gin.Context) {
		filter, limit, anchor, ok := parseChangeEntryQuery(c)
		if !ok {
			return
		}
		if filter.Environment != "" && !requireEnvironment(c, deps, filter.Environment) {
			return
		}
		entries, err := deps.Store.ListChangeEntries(filter, anchor, limit)
		if err != nil {
			fail(c, http.StatusServiceUnavailable, store.CodeStorageUnavailable, "database is not available")
			return
		}
		nextCursor := ""
		if len(entries) > limit {
			last := entries[limit-1]
			nextCursor = encodeChangeEntryCursor(changeEntryCursor{
				RecordedAt:  last.RecordedAt,
				RecordID:    last.RecordID,
				Sequence:    last.Sequence,
				Title:       last.Title,
				EntryID:     last.ID,
				Environment: filter.Environment,
				Version:     filter.Version,
				BatchID:     filter.BatchID,
				Category:    filter.Category,
				EntryFilter: filter.Title,
				GateStatus:  filter.GateStatus,
				From:        filter.From,
				To:          filter.To,
			})
			entries = entries[:limit]
		}
		items := make([]changeEntryItemView, 0, len(entries))
		for _, entry := range entries {
			items = append(items, toChangeEntryItemView(entry))
		}
		c.JSON(http.StatusOK, changeEntriesPageView{Changes: items, NextCursor: nextCursor})
	}
}

func toChangeEntryItemView(entry store.ChangeEntryRecord) changeEntryItemView {
	return changeEntryItemView{
		Sequence:      entry.Sequence,
		Category:      entry.Category,
		Title:         entry.Title,
		Description:   entry.Description,
		ID:            entry.PublicID,
		Environment:   entry.Environment,
		Version:       entry.Version,
		BatchID:       entry.BatchID,
		GateStatus:    entry.GateStatus,
		RollbackPoint: entry.RollbackPoint,
		RecordedAt:    entry.RecordedAt,
	}
}

// parseChangeEntryTime accepts only the documented second-precision UTC
// shape. Fractional seconds and non-UTC offsets are rejected even when Go's
// permissive time parser would accept them.
func parseChangeEntryTime(raw string) (string, error) {
	instant, err := time.Parse("2006-01-02T15:04:05Z", raw)
	if err != nil {
		return "", err
	}
	canonical := instant.UTC().Format("2006-01-02T15:04:05Z")
	if raw != canonical {
		return "", errNonCanonicalTime
	}
	return canonical, nil
}
