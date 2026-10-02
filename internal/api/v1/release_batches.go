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

	"github.com/gin-gonic/gin"

	"github.com/luwa07832/deployment-tracker/internal/store"
)

const (
	defaultReleaseBatchLimit = 20
	maxReleaseBatchLimit     = 100
)

// releaseBatchCursorSigningKey signs opaque pagination cursors so a
// truncated, forged or condition-rebound cursor cannot be mistaken for a
// valid one. The key is process-private randomness; cursors never need to
// survive a restart.
var releaseBatchCursorSigningKey = newReleaseBatchCursorSigningKey()

func newReleaseBatchCursorSigningKey() []byte {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic(err)
	}
	return key
}

// gateCountsView is the per-batch record count by gate status.
type gateCountsView struct {
	Allowed int `json:"allowed"`
	Blocked int `json:"blocked"`
	Pending int `json:"pending"`
}

// releaseBatchSummaryView is one batch discovery row. Summary counts cover
// every release record of the batch, including records outside the
// filter-matching window.
type releaseBatchSummaryView struct {
	BatchID        string         `json:"batch_id"`
	ReleaseCount   int            `json:"release_count"`
	Environments   []string       `json:"environments"`
	ChangeCount    int            `json:"change_count"`
	GateCounts     gateCountsView `json:"gate_counts"`
	LastRecordedAt string         `json:"last_recorded_at"`
}

// releaseBatchDetailView is the single-batch response: the same summary plus
// every complete release record of the batch.
type releaseBatchDetailView struct {
	releaseBatchSummaryView
	Releases []recordView `json:"releases"`
}

// releaseBatchQuery is the validated, normalized query shape shared by the
// discovery handler and its cursor binding.
type releaseBatchQuery struct {
	Environment string
	Version     string
	GateStatus  string
	From        string
	To          string
}

type releaseBatchCursorFilter struct {
	Environment string `json:"environment"`
	Version     string `json:"version"`
	GateStatus  string `json:"gate_status"`
	From        string `json:"from"`
	To          string `json:"to"`
}

type releaseBatchCursor struct {
	Filter         releaseBatchCursorFilter `json:"f"`
	LastRecordedAt string                   `json:"last_recorded_at"`
	BatchID        string                   `json:"batch_id"`
}

func (query releaseBatchQuery) cursorFilter() releaseBatchCursorFilter {
	return releaseBatchCursorFilter{
		Environment: query.Environment,
		Version:     query.Version,
		GateStatus:  query.GateStatus,
		From:        query.From,
		To:          query.To,
	}
}

func (query releaseBatchQuery) storeFilter() store.ReleaseBatchFilter {
	return store.ReleaseBatchFilter{
		Environment: query.Environment,
		Version:     query.Version,
		GateStatus:  query.GateStatus,
		From:        query.From,
		To:          query.To,
	}
}

func encodeReleaseBatchCursor(cursor releaseBatchCursor) string {
	payload, _ := json.Marshal(cursor)
	mac := hmac.New(sha256.New, releaseBatchCursorSigningKey)
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// decodeReleaseBatchCursor validates shape, signature and filter binding of
// a cursor. Any tampering, truncation or condition mismatch yields false.
func decodeReleaseBatchCursor(raw string, expected releaseBatchCursorFilter) (releaseBatchCursor, bool) {
	parts := strings.Split(strings.TrimSpace(raw), ".")
	if len(parts) != 2 {
		return releaseBatchCursor{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return releaseBatchCursor{}, false
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return releaseBatchCursor{}, false
	}
	mac := hmac.New(sha256.New, releaseBatchCursorSigningKey)
	mac.Write(payload)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return releaseBatchCursor{}, false
	}
	var cursor releaseBatchCursor
	if err := json.Unmarshal(payload, &cursor); err != nil {
		return releaseBatchCursor{}, false
	}
	if cursor.Filter != expected || cursor.LastRecordedAt == "" || cursor.BatchID == "" {
		return releaseBatchCursor{}, false
	}
	return cursor, true
}

func toReleaseBatchSummaryView(summary store.ReleaseBatchSummary) releaseBatchSummaryView {
	return releaseBatchSummaryView{
		BatchID:        summary.BatchID,
		ReleaseCount:   summary.ReleaseCount,
		Environments:   append([]string{}, summary.Environments...),
		ChangeCount:    summary.ChangeCount,
		GateCounts:     gateCountsView(summary.GateCounts),
		LastRecordedAt: summary.LastRecordedAt,
	}
}

func toReleaseBatchDetailView(summary store.ReleaseBatchSummary) releaseBatchDetailView {
	return releaseBatchDetailView{
		releaseBatchSummaryView: toReleaseBatchSummaryView(summary),
		Releases:                toRecordViews(summary.Releases),
	}
}

// listReleaseBatches answers GET /api/v1/release-batches: a filtered,
// keyset-paginated discovery view over non-empty release batches.
func listReleaseBatches(deps Dependencies) gin.HandlerFunc {
	return func(c *gin.Context) {
		query, limit, ok := parseReleaseBatchQuery(c)
		if !ok {
			return
		}

		var anchorAt, anchorBatchID string
		if raw := strings.TrimSpace(c.Query("cursor")); raw != "" {
			cursor, valid := decodeReleaseBatchCursor(raw, query.cursorFilter())
			if !valid {
				fail(c, http.StatusBadRequest, store.CodeBatchQueryInvalidV1,
					"cursor is not valid for this query")
				return
			}
			anchorAt, anchorBatchID = cursor.LastRecordedAt, cursor.BatchID
		}

		if query.Environment != "" && !requireEnvironment(c, deps, query.Environment) {
			return
		}

		summaries, err := deps.Store.ListReleaseBatches(query.storeFilter(), anchorAt, anchorBatchID, limit)
		if err != nil {
			fail(c, http.StatusServiceUnavailable, store.CodeStorageUnavailable,
				"database is not available")
			return
		}

		var nextCursor any
		if len(summaries) > limit {
			last := summaries[limit-1]
			nextCursor = encodeReleaseBatchCursor(releaseBatchCursor{
				Filter:         query.cursorFilter(),
				LastRecordedAt: last.LastRecordedAt,
				BatchID:        last.BatchID,
			})
			summaries = summaries[:limit]
		}
		views := make([]releaseBatchSummaryView, 0, len(summaries))
		for _, summary := range summaries {
			views = append(views, toReleaseBatchSummaryView(summary))
		}
		c.JSON(http.StatusOK, gin.H{"batches": views, "next_cursor": nextCursor})
	}
}

// getReleaseBatch answers GET /api/v1/release-batches/{batch_id}: one batch
// summary with every complete release record of the batch.
func getReleaseBatch(deps Dependencies) gin.HandlerFunc {
	return func(c *gin.Context) {
		batchID := strings.TrimSpace(c.Param("batch_id"))
		summary, err := deps.Store.GetReleaseBatch(batchID)
		if err != nil {
			fail(c, http.StatusServiceUnavailable, store.CodeStorageUnavailable,
				"database is not available")
			return
		}
		if summary == nil {
			fail(c, http.StatusNotFound, store.CodeReleaseBatchNotFoundV1,
				"no release batch with id "+batchID)
			return
		}
		c.JSON(http.StatusOK, gin.H{"batch": toReleaseBatchDetailView(*summary)})
	}
}

// parseReleaseBatchQuery validates every discovery condition before any
// storage access: blank parameters, gate-status enumeration, canonical UTC
// second-precision bounds, bound ordering and limit range.
func parseReleaseBatchQuery(c *gin.Context) (releaseBatchQuery, int, bool) {
	get := func(key string) (string, bool) {
		raw, present := c.GetQuery(key)
		if present && strings.TrimSpace(raw) == "" {
			fail(c, http.StatusBadRequest, store.CodeBatchQueryInvalidV1,
				key+" must not be blank when provided")
			return "", false
		}
		return strings.TrimSpace(raw), true
	}

	environment, ok := get("environment")
	if !ok {
		return releaseBatchQuery{}, 0, false
	}
	version, ok := get("version")
	if !ok {
		return releaseBatchQuery{}, 0, false
	}
	gateStatus, ok := get("gate_status")
	if !ok {
		return releaseBatchQuery{}, 0, false
	}
	if gateStatus != "" && !validGateStatuses[gateStatus] {
		fail(c, http.StatusBadRequest, store.CodeBatchQueryInvalidV1,
			"gate_status must be one of: allowed, blocked, pending")
		return releaseBatchQuery{}, 0, false
	}
	from, ok := get("from")
	if !ok {
		return releaseBatchQuery{}, 0, false
	}
	if from != "" {
		parsed, valid := parseSecondPrecisionUTC(from)
		if !valid {
			fail(c, http.StatusBadRequest, store.CodeBatchQueryInvalidV1,
				"from must be a UTC second-precision timestamp in YYYY-MM-DDTHH:MM:SSZ format")
			return releaseBatchQuery{}, 0, false
		}
		from = parsed
	}
	to, ok := get("to")
	if !ok {
		return releaseBatchQuery{}, 0, false
	}
	if to != "" {
		parsed, valid := parseSecondPrecisionUTC(to)
		if !valid {
			fail(c, http.StatusBadRequest, store.CodeBatchQueryInvalidV1,
				"to must be a UTC second-precision timestamp in YYYY-MM-DDTHH:MM:SSZ format")
			return releaseBatchQuery{}, 0, false
		}
		to = parsed
	}
	if from != "" && to != "" && from > to {
		fail(c, http.StatusBadRequest, store.CodeBatchQueryInvalidV1,
			"from must not be later than to")
		return releaseBatchQuery{}, 0, false
	}
	limit := defaultReleaseBatchLimit
	if raw, present := c.GetQuery("limit"); present {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			fail(c, http.StatusBadRequest, store.CodeBatchQueryInvalidV1,
				"limit must not be blank when provided")
			return releaseBatchQuery{}, 0, false
		}
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > maxReleaseBatchLimit {
			fail(c, http.StatusBadRequest, store.CodeBatchQueryInvalidV1,
				"limit must be an integer between 1 and "+strconv.Itoa(maxReleaseBatchLimit))
			return releaseBatchQuery{}, 0, false
		}
		limit = parsed
	}
	if raw, present := c.GetQuery("cursor"); present && strings.TrimSpace(raw) == "" {
		fail(c, http.StatusBadRequest, store.CodeBatchQueryInvalidV1,
			"cursor must not be blank when provided")
		return releaseBatchQuery{}, 0, false
	}
	return releaseBatchQuery{
		Environment: environment,
		Version:     version,
		GateStatus:  gateStatus,
		From:        from,
		To:          to,
	}, limit, true
}
