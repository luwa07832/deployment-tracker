package v1

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/luwa07832/deployment-tracker/internal/store"
)

// fetchRecordListPage requests one page and returns the ids in page order
// plus the next cursor.
func fetchRecordListPage(t *testing.T, router *gin.Engine, rawQuery string) ([]string, string) {
	t.Helper()
	recorder := doRequest(t, router, http.MethodGet, "/api/v1/release-records?"+rawQuery, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	body := decodeBody(t, recorder)
	rawList, present := body["release_records"]
	if !present {
		t.Fatalf("response missing release_records: %s", recorder.Body.String())
	}
	list := rawList.([]any)
	ids := make([]string, 0, len(list))
	for _, item := range list {
		ids = append(ids, item.(map[string]any)["id"].(string))
	}
	next, _ := body["next_cursor"].(string)
	if len(body) != 2 {
		t.Fatalf("response must contain only release_records and next_cursor: %s", recorder.Body.String())
	}
	return ids, next
}

func createVersionedRecords(t *testing.T, router *gin.Engine, count int) []string {
	t.Helper()
	ids := make([]string, 0, count)
	for i := 0; i < count; i++ {
		body := recordBody("prod", fmt.Sprintf("9.%d.0", i), "allowed", "snapshot:0.9.0",
			entry(1, "feature", "change", "description"))
		record := createRecordOK(t, router, body)
		ids = append(ids, record["id"].(string))
	}
	return ids
}

func TestListRecordsPaginatesNewestFirstWithStableCursor(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "prod")
	ids := createVersionedRecords(t, router, 5)

	first, cursor := fetchRecordListPage(t, router, "limit=2")
	if strings.Join(first, ",") != strings.Join([]string{ids[4], ids[3]}, ",") {
		t.Fatalf("first page = %v", first)
	}
	if cursor == "" {
		t.Fatalf("expected next_cursor after a partial page")
	}
	firstCursor := cursor

	second, cursor := fetchRecordListPage(t, router, "limit=2&cursor="+url.QueryEscape(cursor))
	if strings.Join(second, ",") != strings.Join([]string{ids[2], ids[1]}, ",") {
		t.Fatalf("second page = %v", second)
	}

	// The next page may carry a different limit.
	third, cursor := fetchRecordListPage(t, router, "limit=1&cursor="+url.QueryEscape(cursor))
	if strings.Join(third, ",") != ids[0] {
		t.Fatalf("third page = %v", third)
	}
	if cursor != "" {
		t.Fatalf("last page cursor = %q, want empty", cursor)
	}

	// Repeating a request with the same cursor and data yields the same
	// subsequent page.
	replay, replayCursor := fetchRecordListPage(t, router, "limit=2&cursor="+url.QueryEscape(firstCursor))
	replayAgain, _ := fetchRecordListPage(t, router, "limit=2&cursor="+url.QueryEscape(firstCursor))
	if strings.Join(replay, ",") != strings.Join(replayAgain, ",") || replayCursor == "" {
		t.Fatalf("cursor replay = %v vs %v", replay, replayAgain)
	}
}

func TestListRecordsDefaultLimitAndEmptyResult(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "prod")

	ids, cursor := fetchRecordListPage(t, router, "")
	if len(ids) != 0 || cursor != "" {
		t.Fatalf("empty result = %v, %q", ids, cursor)
	}

	all := createVersionedRecords(t, router, 25)
	page, next := fetchRecordListPage(t, router, "")
	if len(page) != 20 || next == "" {
		t.Fatalf("default limit page size = %d, cursor %q", len(page), next)
	}
	if strings.Join(page, ",") != strings.Join(reverseIDs(all[5:]), ",") {
		t.Fatalf("default first page order mismatch: %v", page)
	}
	rest, final := fetchRecordListPage(t, router, "cursor="+url.QueryEscape(next))
	if len(rest) != 5 || final != "" {
		t.Fatalf("last page = %d records, cursor %q", len(rest), final)
	}
}

func TestListRecordsCursorOpaqueAndBoundToFilter(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "prod")
	registerEnvironmentOK(t, router, "staging")
	createVersionedRecords(t, router, 3)
	createRecordOK(t, router, recordBody("staging", "8.0.0", "blocked", "snapshot:0.8.0",
		entry(1, "fix", "s", "d")))

	_, cursor := fetchRecordListPage(t, router, "environment=prod&limit=1")
	for _, label := range []string{"not base64!", "YWJj.DQ", strings.Repeat("a", 64), cursor[:len(cursor)-2]} {
		recorder := doRequest(t, router, http.MethodGet,
			"/api/v1/release-records?environment=prod&cursor="+url.QueryEscape(label), "")
		wantError(t, recorder, http.StatusBadRequest, store.CodeInvalidReleaseRecordQueryV1)
	}

	// A cursor minted for one filter cannot page another query.
	for _, query := range []string{
		"environment=staging",
		"environment=prod&gate_status=blocked",
		"environment=prod&recorded_to=2000-01-01",
		"",
	} {
		recorder := doRequest(t, router, http.MethodGet,
			"/api/v1/release-records?"+query+"&cursor="+url.QueryEscape(cursor), "")
		wantError(t, recorder, http.StatusBadRequest, store.CodeInvalidReleaseRecordQueryV1)
	}

	// The token must not expose the internal primary key.
	if strings.Contains(strings.ToLower(cursor), "recorded_at") || strings.Contains(cursor, "\"") {
		t.Fatalf("cursor leaks payload structure: %s", cursor)
	}
}

func TestListRecordsQueryValidation(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "prod")

	for _, query := range []string{
		"limit=", "limit=%20", "limit=x", "limit=0", "limit=101", "limit=-1", "limit=1.5",
		"cursor=", "cursor=%09",
	} {
		recorder := doRequest(t, router, http.MethodGet, "/api/v1/release-records?"+query, "")
		wantError(t, recorder, http.StatusBadRequest, store.CodeInvalidReleaseRecordQueryV1)
	}

	// Existing validation semantics are unchanged.
	wantError(t, doRequest(t, router, http.MethodGet,
		"/api/v1/release-records?environment=ghost", ""),
		http.StatusNotFound, store.CodeEnvironmentNotFoundV1)
	wantError(t, doRequest(t, router, http.MethodGet,
		"/api/v1/release-records?gate_status=nope", ""),
		http.StatusUnprocessableEntity, store.CodeReleaseValidationV1)
	wantError(t, doRequest(t, router, http.MethodGet,
		"/api/v1/release-records?recorded_from=tuesday", ""),
		http.StatusUnprocessableEntity, store.CodeReleaseValidationV1)
}

func TestListRecordsCursorAcceptsCanonicalTimeEquivalent(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "prod")
	createVersionedRecords(t, router, 2)

	// The date bound canonicalizes to the end of that UTC day, so a cursor
	// minted with the date form is also valid for the instant form and vice
	// versa. The opaque token itself differs because it carries random
	// encryption nonce, but both bind the same filter snapshot.
	_, dateCursor := fetchRecordListPage(t, router, "limit=1&recorded_to=2999-01-01")
	fetchRecordListPage(t, router,
		"limit=1&recorded_to=2999-01-01T23:59:59Z&cursor="+url.QueryEscape(dateCursor))

	_, instantCursor := fetchRecordListPage(t, router,
		"limit=1&recorded_from=2000-01-01T00:00:00Z")
	fetchRecordListPage(t, router,
		"limit=1&recorded_from=2000-01-01&cursor="+url.QueryEscape(instantCursor))
}

func reverseIDs(ids []string) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[len(ids)-1-i] = id
	}
	return out
}
