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

// releaseBatchCursorSigningKey signs opaque batch pagination cursors so a
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

type gateCountsView struct {
	Allowed int `json:"allowed"`
	Blocked int `json:"blocked"`
	Pending int `json:"pending"`
}

type releaseBatchSummaryView struct {
	BatchID        string         `json:"batch_id"`
	ReleaseCount   int            `json:"release_count"`
	Environments   []string       `json:"environments"`
	ChangeCount    int            `json:"change_count"`
	GateCounts     gateCountsView `json:"gate_counts"`
	LastRecordedAt string         `json:"last_recorded_at"`
}

type releaseBatchDetailView struct {
	releaseBatchSummaryView
	Releases []recordView `json:"releases"`
}

func toReleaseBatchSummaryView(
	summary *store.ReleaseBatchSummary,
	environments []string,
) releaseBatchSummaryView {
	return releaseBatchSummaryView{
		BatchID:      summary.BatchID,
		ReleaseCount: summary.ReleaseCount,
		Environments: environments,
		ChangeCount:  summary.ChangeCount,
		GateCounts: gateCountsView{
			Allowed: summary.GateAllowed,
			Blocked: summary.GateBlocked,
			Pending: summary.GatePending,
		},
		LastRecordedAt: summary.LastRecordedAt,
	}
}

// releaseBatchCursorFilter is the normalized snapshot of the five filter
// conditions taken by the first request and bound to every cursor it mints.
type releaseBatchCursorFilter struct {
	Environment string `json:"environment"`
	Version     string `json:"version"`
	GateStatus  string `json:"gate_status"`
	From        string `json:"from"`
	To          string `json:"to"`
}

// releaseBatchCursor is the signed, opaque keyset marker. The position is the
// summary ordering tuple (last_recorded_at, batch_id).
type releaseBatchCursor struct {
	Version    int                      `json:"v"`
	Filter     releaseBatchCursorFilter `json:"f"`
	RecordedAt string                   `json:"recorded_at"`
	BatchID    string                   `json:"batch_id"`
}

func encodeReleaseBatchCursor(cursor releaseBatchCursor) string {
	payload, _ := json.Marshal(cursor)
	mac := hmac.New(sha256.New, releaseBatchCursorSigningKey)
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// decodeReleaseBatchCursor validates the shape, signature and filter binding
// of a cursor. Any tampering, truncation, missing binding or condition
// mismatch yields false.
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
	if cursor.Version != 1 || cursor.Filter != expected ||
		cursor.RecordedAt == "" || cursor.BatchID == "" {
		return releaseBatchCursor{}, false
	}
	return cursor, true
}

// listReleaseBatches answers GET /api/v1/release-batches: a read-only,
// AND-filtered discovery view of release batches with stable keyset pages.
func listReleaseBatches(deps Dependencies) gin.HandlerFunc {
	return func(c *gin.Context) {
		filter := store.ReleaseBatchFilter{
			Environment: strings.TrimSpace(c.Query("environment")),
			Version:     strings.TrimSpace(c.Query("version")),
			GateStatus:  strings.TrimSpace(c.Query("gate_status")),
		}

		// Query-shape validation happens before any storage lookup.
		limit := defaultReleaseBatchLimit
		if raw, present := c.GetQuery("limit"); present {
			raw = strings.TrimSpace(raw)
			if raw == "" {
				fail(c, http.StatusBadRequest, store.CodeBatchQueryInvalidV1,
					"limit must not be blank when provided")
				return
			}
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed < 1 || parsed > maxReleaseBatchLimit {
				fail(c, http.StatusBadRequest, store.CodeBatchQueryInvalidV1,
					"limit must be an integer between 1 and "+strconv.Itoa(maxReleaseBatchLimit))
				return
			}
			limit = parsed
		}
		if raw, present := c.GetQuery("cursor"); present && strings.TrimSpace(raw) == "" {
			fail(c, http.StatusBadRequest, store.CodeBatchQueryInvalidV1,
				"cursor must not be blank when provided")
			return
		}
		if filter.GateStatus != "" && !validGateStatuses[filter.GateStatus] {
			fail(c, http.StatusBadRequest, store.CodeBatchQueryInvalidV1,
				"gate_status must be one of: allowed, blocked, pending")
			return
		}
		if raw := strings.TrimSpace(c.Query("from")); raw != "" {
			bound, err := parseTimeBound(raw, false)
			if err != nil {
				fail(c, http.StatusBadRequest, store.CodeBatchQueryInvalidV1,
					"from must be an RFC3339 timestamp or a YYYY-MM-DD date")
				return
			}
			filter.From = bound
		}
		if raw := strings.TrimSpace(c.Query("to")); raw != "" {
			bound, err := parseTimeBound(raw, true)
			if err != nil {
				fail(c, http.StatusBadRequest, store.CodeBatchQueryInvalidV1,
					"to must be an RFC3339 timestamp or a YYYY-MM-DD date")
				return
			}
			filter.To = bound
		}

		if filter.Environment != "" && !requireEnvironment(c, deps, filter.Environment) {
			return
		}

		cursorFilter := releaseBatchCursorFilter{
			Environment: filter.Environment,
			Version:     filter.Version,
			GateStatus:  filter.GateStatus,
			From:        filter.From,
			To:          filter.To,
		}
		var anchorRecordedAt string
		var anchorBatchID string
		if raw := strings.TrimSpace(c.Query("cursor")); raw != "" {
			cursor, valid := decodeReleaseBatchCursor(raw, cursorFilter)
			if !valid {
				fail(c, http.StatusBadRequest, store.CodeBatchQueryInvalidV1,
					"cursor is not valid for this query")
				return
			}
			anchorRecordedAt = cursor.RecordedAt
			anchorBatchID = cursor.BatchID
		}

		summaries, err := deps.Store.ListReleaseBatchesPage(filter, anchorRecordedAt, anchorBatchID, limit)
		if err != nil {
			failStorage(c)
			return
		}

		var nextCursor any
		if len(summaries) > limit {
			last := summaries[limit-1]
			nextCursor = encodeReleaseBatchCursor(releaseBatchCursor{
				Version:    1,
				Filter:     cursorFilter,
				RecordedAt: last.LastRecordedAt,
				BatchID:    last.BatchID,
			})
			summaries = summaries[:limit]
		}

		batches := make([]releaseBatchSummaryView, 0, len(summaries))
		for i := range summaries {
			environments, err := deps.Store.ListReleaseBatchEnvironments(summaries[i].BatchID)
			if err != nil {
				failStorage(c)
				return
			}
			batches = append(batches, toReleaseBatchSummaryView(&summaries[i], environments))
		}
		c.JSON(http.StatusOK, gin.H{"batches": batches, "next_cursor": nextCursor})
	}
}

// getReleaseBatch answers GET /api/v1/release-batches/:batch_id: one exact
// batch summary plus every complete release record of the batch in
// chronological order.
func getReleaseBatch(deps Dependencies) gin.HandlerFunc {
	return func(c *gin.Context) {
		batchID := strings.TrimSpace(c.Param("batch_id"))
		if batchID == "" {
			fail(c, http.StatusNotFound, store.CodeReleaseBatchNotFoundV1, "no release batch with id ")
			return
		}
		summary, err := deps.Store.GetReleaseBatchSummary(batchID)
		if err != nil {
			failStorage(c)
			return
		}
		if summary == nil {
			fail(c, http.StatusNotFound, store.CodeReleaseBatchNotFoundV1,
				"no release batch with id "+batchID)
			return
		}
		environments, err := deps.Store.ListReleaseBatchEnvironments(batchID)
		if err != nil {
			failStorage(c)
			return
		}
		records, err := deps.Store.ListReleaseBatchRecords(batchID)
		if err != nil {
			failStorage(c)
			return
		}
		view := releaseBatchDetailView{
			releaseBatchSummaryView: toReleaseBatchSummaryView(summary, environments),
			Releases:                toRecordViews(records),
		}
		c.JSON(http.StatusOK, gin.H{"release_batch": view})
	}
}
