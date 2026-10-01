package api

import (
	"encoding/base64"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/luwa07832/deployment-tracker/internal/store"
)

const (
	historyDefaultLimit = 20
	historyMaxLimit     = 100
)

// recordedAtPattern matches the server's UTC second/fraction timestamps,
// e.g. 2026-10-01T08:00:00Z or 2026-10-01T08:00:00.123Z.
var recordedAtPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z$`)

// releaseHistoryPageView is one stable page of one environment's release
// history, newest first. Each entry keeps version, change entries, gate
// status and rollback point, and its id links back to the original record via
// GET /releases/{environment}/{version}.
type releaseHistoryPageView struct {
	Environment string            `json:"environment"`
	Limit       int               `json:"limit"`
	NextCursor  string            `json:"next_cursor,omitempty"`
	Releases    []releaseResponse `json:"releases"`
}

// getReleaseHistoryPage answers GET /environments/{environment}/release-history.
// Paging is keyset-based over (registered time desc, registration id desc),
// so pages never repeat or skip recorded releases, even when new releases
// arrive while a caller pages.
func getReleaseHistoryPage(deps Dependencies) gin.HandlerFunc {
	return func(c *gin.Context) {
		environment := c.Param("environment")
		if !environmentExists(c, deps, environment) {
			return
		}
		limit := historyDefaultLimit
		if raw := c.Query("limit"); raw != "" {
			value, err := strconv.Atoi(raw)
			if err != nil || value <= 0 || value > historyMaxLimit {
				fail(c, http.StatusBadRequest, store.CodeInvalidRequest,
					"limit must be an integer between 1 and "+strconv.Itoa(historyMaxLimit))
				return
			}
			limit = value
		}
		var afterID int64
		var afterAt string
		if rawCursor := c.Query("cursor"); rawCursor != "" {
			id, at, ok := decodeHistoryCursor(c, rawCursor)
			if !ok {
				return
			}
			afterID, afterAt = id, at
		}
		page, err := deps.Store.ListReleaseHistory(environment, limit, afterID, afterAt)
		if err != nil {
			fail(c, http.StatusInternalServerError, store.CodeStorageUnavailable, "database is not available")
			return
		}
		view := releaseHistoryPageView{
			Environment: environment,
			Limit:       limit,
			Releases:    toReleaseList(page.Releases),
		}
		if page.HasNext {
			view.NextCursor = encodeHistoryCursor(page.NextLastID, page.NextLastAt)
		}
		c.JSON(http.StatusOK, view)
	}
}

// encodeHistoryCursor packs the last seen (id, registered_at) position into an
// opaque URL-safe token. Clients treat it as opaque and only pass it back.
func encodeHistoryCursor(id int64, recordedAt string) string {
	payload := strconv.FormatInt(id, 10) + "|" + recordedAt
	return base64.RawURLEncoding.EncodeToString([]byte(payload))
}

// decodeHistoryCursor validates the token shape. Any malformed value is a
// deterministic invalid_request rather than an empty first page.
func decodeHistoryCursor(c *gin.Context, token string) (int64, string, bool) {
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(token))
	if err != nil {
		fail(c, http.StatusBadRequest, store.CodeInvalidRequest, "cursor is not a valid pagination cursor")
		return 0, "", false
	}
	parts := strings.SplitN(string(decoded), "|", 2)
	if len(parts) != 2 {
		fail(c, http.StatusBadRequest, store.CodeInvalidRequest, "cursor is not a valid pagination cursor")
		return 0, "", false
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || id <= 0 || !recordedAtPattern.MatchString(parts[1]) {
		fail(c, http.StatusBadRequest, store.CodeInvalidRequest, "cursor is not a valid pagination cursor")
		return 0, "", false
	}
	return id, parts[1], true
}
