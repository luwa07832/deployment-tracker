package v1

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
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

// changeCursorSigningKey signs opaque pagination cursors so a truncated,
// forged or condition-rebound cursor cannot be mistaken for a valid one. The
// key is process-private randomness; cursors never need to survive a restart.
var changeCursorSigningKey = newChangeCursorSigningKey()

func newChangeCursorSigningKey() []byte {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic(err)
	}
	return key
}

// changeEntryFilterView holds the validated query conditions.
type changeEntryFilterView struct {
	Environment string
	Version     string
	BatchID     string
	Category    string
	Title       string
	GateStatus  string
	From        string
	To          string
}

// changeEntryCursor is the signed, opaque keyset marker. It binds the filter
// conditions to the position so a cursor minted for one query cannot page
// another.
type changeEntryCursor struct {
	Filter changeCursorFilter `json:"f"`
	Pos    changeCursorPos    `json:"p"`
}

type changeCursorFilter struct {
	Environment string `json:"environment"`
	Version     string `json:"version"`
	BatchID     string `json:"batch_id"`
	Category    string `json:"category"`
	Title       string `json:"title"`
	GateStatus  string `json:"gate_status"`
	From        string `json:"from"`
	To          string `json:"to"`
}

type changeCursorPos struct {
	RecordedAt string `json:"recorded_at"`
	RecordID   int64  `json:"record_id"`
	Sequence   int    `json:"sequence"`
	Title      string `json:"title"`
}

func (f changeEntryFilterView) cursorFilter() changeCursorFilter {
	return changeCursorFilter{
		Environment: f.Environment,
		Version:     f.Version,
		BatchID:     f.BatchID,
		Category:    f.Category,
		Title:       f.Title,
		GateStatus:  f.GateStatus,
		From:        f.From,
		To:          f.To,
	}
}

func encodeChangeCursor(cursor changeEntryCursor) string {
	payload, _ := json.Marshal(cursor)
	mac := hmac.New(sha256.New, changeCursorSigningKey)
	mac.Write(payload)
	signature := mac.Sum(nil)
	return base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(signature)
}

// decodeChangeCursor validates the shape, signature and filter binding of a
// cursor. Any tampering or mismatch yields ok == false.
func decodeChangeCursor(raw string, expected changeCursorFilter) (changeCursorPos, bool) {
	parts := strings.Split(strings.TrimSpace(raw), ".")
	if len(parts) != 2 {
		return changeCursorPos{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return changeCursorPos{}, false
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return changeCursorPos{}, false
	}
	mac := hmac.New(sha256.New, changeCursorSigningKey)
	mac.Write(payload)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return changeCursorPos{}, false
	}
	var cursor changeEntryCursor
	if err := json.Unmarshal(payload, &cursor); err != nil {
		return changeCursorPos{}, false
	}
	if cursor.Filter != expected {
		return changeCursorPos{}, false
	}
	if cursor.Pos.RecordedAt == "" || cursor.Pos.RecordID <= 0 || cursor.Pos.Sequence <= 0 ||
		cursor.Pos.Title == "" {
		return changeCursorPos{}, false
	}
	return cursor.Pos, true
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

func toChangeEntryItemView(item store.ChangeEntryItem) changeEntryItemView {
	return changeEntryItemView{
		Sequence:      item.Entry.Sequence,
		Category:      item.Entry.Category,
		Title:         item.Entry.Title,
		Description:   item.Entry.Description,
		ID:            item.PublicID,
		Environment:   item.Environment,
		Version:       item.Version,
		BatchID:       item.BatchID,
		GateStatus:    item.GateStatus,
		RollbackPoint: item.RollbackPoint,
		RecordedAt:    item.RecordedAt,
	}
}

// listChangeEntries answers GET /api/v1/change-entries: a filtered,
// keyset-paginated view over the structured changes written through
// POST /api/v1/release-records only.
func listChangeEntries(deps Dependencies) gin.HandlerFunc {
	return func(c *gin.Context) {
		filter, ok := parseChangeEntryFilter(c)
		if !ok {
			return
		}

		var anchor store.ChangeEntryItem
		if raw := strings.TrimSpace(c.Query("cursor")); raw != "" {
			pos, valid := decodeChangeCursor(raw, filter.cursorFilter())
			if !valid {
				fail(c, http.StatusBadRequest, store.CodeInvalidChangeQueryV1,
					"cursor is not valid for this query")
				return
			}
			anchor.RecordedAt = pos.RecordedAt
			anchor.RecordID = pos.RecordID
			anchor.Entry.Sequence = pos.Sequence
			anchor.Entry.Title = pos.Title
		}

		limit := defaultChangeEntryLimit
		if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed < 1 || parsed > maxChangeEntryLimit {
				fail(c, http.StatusBadRequest, store.CodeInvalidChangeQueryV1,
					"limit must be an integer between 1 and 100")
				return
			}
			limit = parsed
		}
		if filter.Environment != "" {
			exists, err := deps.Store.TrackedEnvironmentExists(filter.Environment)
			if err != nil {
				fail(c, http.StatusServiceUnavailable, store.CodeStorageUnavailable,
					"database is not available")
				return
			}
			if !exists {
				fail(c, http.StatusNotFound, store.CodeEnvironmentNotFoundV1,
					"no such environment "+filter.Environment)
				return
			}
		}

		items, err := deps.Store.ListChangeEntries(store.ChangeEntryFilter{
			Environment: filter.Environment,
			Version:     filter.Version,
			BatchID:     filter.BatchID,
			Category:    filter.Category,
			Title:       filter.Title,
			GateStatus:  filter.GateStatus,
			From:        filter.From,
			To:          filter.To,
		}, anchor, limit)
		if err != nil {
			fail(c, http.StatusServiceUnavailable, store.CodeStorageUnavailable,
				"database is not available")
			return
		}

		nextCursor := ""
		if len(items) > limit {
			last := items[limit-1]
			nextCursor = encodeChangeCursor(changeEntryCursor{
				Filter: filter.cursorFilter(),
				Pos: changeCursorPos{
					RecordedAt: last.RecordedAt,
					RecordID:   last.RecordID,
					Sequence:   last.Entry.Sequence,
					Title:      last.Entry.Title,
				},
			})
			items = items[:limit]
		}
		views := make([]changeEntryItemView, 0, len(items))
		for _, item := range items {
			views = append(views, toChangeEntryItemView(item))
		}
		c.JSON(http.StatusOK, gin.H{"changes": views, "next_cursor": nextCursor})
	}
}

// parseChangeEntryFilter validates every query condition before any storage
// access: blank parameters, gate-status enumeration, second-precision UTC
// bounds and from/at ordering. Storage-side cursor decoding happens after
// this in the handler because it needs the canonical filter.
func parseChangeEntryFilter(c *gin.Context) (changeEntryFilterView, bool) {
	get := func(key string) (string, bool) {
		raw, present := c.GetQuery(key)
		if present && strings.TrimSpace(raw) == "" {
			fail(c, http.StatusBadRequest, store.CodeInvalidChangeQueryV1,
				key+" must not be blank when provided")
			return "", false
		}
		return strings.TrimSpace(raw), true
	}

	environment, ok := get("environment")
	if !ok {
		return changeEntryFilterView{}, false
	}
	version, ok := get("version")
	if !ok {
		return changeEntryFilterView{}, false
	}
	batchID, ok := get("batch_id")
	if !ok {
		return changeEntryFilterView{}, false
	}
	category, ok := get("category")
	if !ok {
		return changeEntryFilterView{}, false
	}
	title, ok := get("title")
	if !ok {
		return changeEntryFilterView{}, false
	}
	gateStatus, ok := get("gate_status")
	if !ok {
		return changeEntryFilterView{}, false
	}
	if gateStatus != "" && !validGateStatuses[gateStatus] {
		fail(c, http.StatusBadRequest, store.CodeInvalidChangeQueryV1,
			"gate_status must be one of: allowed, blocked, pending")
		return changeEntryFilterView{}, false
	}
	from, ok := get("from")
	if !ok {
		return changeEntryFilterView{}, false
	}
	to, ok := get("to")
	if !ok {
		return changeEntryFilterView{}, false
	}
	if from != "" {
		parsed, valid := parseSecondPrecisionUTC(from)
		if !valid {
			fail(c, http.StatusBadRequest, store.CodeInvalidChangeQueryV1,
				"from must be a UTC second-precision timestamp in YYYY-MM-DDTHH:MM:SSZ format")
			return changeEntryFilterView{}, false
		}
		from = parsed
	}
	if to != "" {
		parsed, valid := parseSecondPrecisionUTC(to)
		if !valid {
			fail(c, http.StatusBadRequest, store.CodeInvalidChangeQueryV1,
				"to must be a UTC second-precision timestamp in YYYY-MM-DDTHH:MM:SSZ format")
			return changeEntryFilterView{}, false
		}
		to = parsed
	}
	if from != "" && to != "" && from > to {
		fail(c, http.StatusBadRequest, store.CodeInvalidChangeQueryV1,
			"from must not be later than to")
		return changeEntryFilterView{}, false
	}
	if _, present := c.GetQuery("limit"); present && strings.TrimSpace(c.Query("limit")) == "" {
		fail(c, http.StatusBadRequest, store.CodeInvalidChangeQueryV1,
			"limit must not be blank when provided")
		return changeEntryFilterView{}, false
	}
	if raw, present := c.GetQuery("cursor"); present && strings.TrimSpace(raw) == "" {
		fail(c, http.StatusBadRequest, store.CodeInvalidChangeQueryV1,
			"cursor must not be blank when provided")
		return changeEntryFilterView{}, false
	}
	return changeEntryFilterView{
		Environment: environment,
		Version:     version,
		BatchID:     batchID,
		Category:    category,
		Title:       title,
		GateStatus:  gateStatus,
		From:        from,
		To:          to,
	}, true
}

// parseSecondPrecisionUTC accepts only canonical UTC second-precision
// timestamps, YYYY-MM-DDTHH:MM:SSZ. It re-formats the value so the filter
// stored in the cursor is canonical regardless of harmless spacing around
// the query value.
func parseSecondPrecisionUTC(raw string) (string, bool) {
	instant, err := time.Parse("2006-01-02T15:04:05Z", raw)
	if err != nil {
		return "", false
	}
	canonical := instant.UTC().Format("2006-01-02T15:04:05Z")
	if canonical != raw {
		return "", false
	}
	return canonical, true
}
