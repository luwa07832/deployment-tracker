package v1

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/luwa07832/deployment-tracker/internal/store"
)

func decodeRecordsResponse(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	body := decodeBody(t, recorder)
	for _, key := range []string{"release_records", "next_cursor"} {
		if _, ok := body[key]; !ok {
			t.Fatalf("response missing %s: %s", key, recorder.Body.String())
		}
	}
	if len(body) != 2 {
		t.Fatalf("response must contain only release_records and next_cursor: %s", recorder.Body.String())
	}
	return body
}

func recordIDs(t *testing.T, body map[string]any) []string {
	t.Helper()
	raw, ok := body["release_records"].([]any)
	if !ok {
		t.Fatalf("release_records is not an array: %v", body["release_records"])
	}
	ids := make([]string, 0, len(raw))
	for _, item := range raw {
		record, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("record is not an object: %v", item)
		}
		ids = append(ids, record["id"].(string))
	}
	return ids
}

// seedPagedRecords writes five records for the walk tests. Versions are
// unique per environment so every insert is accepted.
func TestListRecordsPaginationWalksEveryRecordOnce(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "prod")
	var inserted []string
	for i := 0; i < 5; i++ {
		version := "1.0." + string(rune('0'+i))
		record := createRecordOK(t, router, withVersion(validRecordBody, version))
		inserted = append(inserted, record["id"].(string))
	}
	want := make([]string, len(inserted))
	for i, id := range inserted {
		want[len(inserted)-1-i] = id
	}

	path := "/api/v1/release-records?limit=2"
	var got []string
	cursorCount := 0
	for page := 0; page < 10; page++ {
		recorder := doRequest(t, router, http.MethodGet, path, "")
		if recorder.Code != http.StatusOK {
			t.Fatalf("page %d status = %d, %s", page, recorder.Code, recorder.Body.String())
		}
		body := decodeRecordsResponse(t, recorder)
		ids := recordIDs(t, body)
		if len(ids) > 2 {
			t.Fatalf("page %d returned %d records", page, len(ids))
		}
		got = append(got, ids...)
		next, _ := body["next_cursor"].(string)
		if next == "" {
			if len(ids) != 1 {
				t.Fatalf("last page must hold the remaining one record, got %d", len(ids))
			}
			break
		}
		cursorCount++
		path = "/api/v1/release-records?limit=2&cursor=" + next
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("walk = %v, want newest-first %v", got, want)
	}
	if cursorCount != 2 {
		t.Fatalf("five records with limit 2 need two non-final pages, got %d", cursorCount)
	}
}

func TestListRecordsDefaultLimitAndEmptyResult(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "prod")

	recorder := doRequest(t, router, http.MethodGet, "/api/v1/release-records?environment=prod", "")
	body := decodeRecordsResponse(t, recorder)
	if ids := recordIDs(t, body); len(ids) != 0 {
		t.Fatalf("empty result = %v", ids)
	}
	if body["next_cursor"] != "" {
		t.Fatalf("empty result next_cursor = %v, want empty string", body["next_cursor"])
	}
	if !strings.Contains(recorder.Body.String(), `"release_records":[]`) {
		t.Fatalf("empty result must serialize as []: %s", recorder.Body.String())
	}
}

func TestListRecordsPaginationQueryValidation(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "prod")
	createRecordOK(t, router, validRecordBody)
	createRecordOK(t, router, withVersion(validRecordBody, "1.3.0"))
	invalid := []string{
		"/api/v1/release-records?limit=",
		"/api/v1/release-records?limit=%20%20",
		"/api/v1/release-records?limit=abc",
		"/api/v1/release-records?limit=1.5",
		"/api/v1/release-records?limit=0",
		"/api/v1/release-records?limit=101",
		"/api/v1/release-records?limit=-1",
		"/api/v1/release-records?cursor=",
	}
	for _, path := range invalid {
		wantError(t, doRequest(t, router, http.MethodGet, path, ""),
			http.StatusBadRequest, store.CodeInvalidReleaseRecordQueryV1)
	}
	// Malformed, truncated or forged cursors are 400 regardless of filters.
	wantError(t, doRequest(t, router, http.MethodGet, "/api/v1/release-records?cursor=not-a-cursor", ""),
		http.StatusBadRequest, store.CodeInvalidReleaseRecordQueryV1)
	first := doRequest(t, router, http.MethodGet, "/api/v1/release-records?limit=1", "")
	cursor := decodeRecordsResponse(t, first)["next_cursor"].(string)
	parts := strings.Split(cursor, ".")
	truncated := doRequest(t, router, http.MethodGet, "/api/v1/release-records?limit=1&cursor="+parts[0], "")
	wantError(t, truncated, http.StatusBadRequest, store.CodeInvalidReleaseRecordQueryV1)
	last := parts[0][len(parts[0])-1]
	replacement := byte('A')
	if last == 'A' {
		replacement = 'B'
	}
	forgedPayload := parts[0][:len(parts[0])-1] + string(rune(replacement))
	forged := doRequest(t, router, http.MethodGet,
		"/api/v1/release-records?limit=1&cursor="+forgedPayload+"."+parts[1], "")
	wantError(t, forged, http.StatusBadRequest, store.CodeInvalidReleaseRecordQueryV1)
}

func TestListRecordsCursorBindsToFilters(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "prod")
	registerEnvironmentOK(t, router, "dev")
	createRecordOK(t, router, withVersion(validRecordBody, "1.0.0"))
	createRecordOK(t, router, withVersion(validRecordBody, "1.0.2"))
	createRecordOK(t, router, strings.ReplaceAll(withVersion(validRecordBody, "1.0.1"), `"prod"`, `"dev"`))

	first := doRequest(t, router, http.MethodGet, "/api/v1/release-records?limit=1&environment=prod", "")
	cursor := decodeRecordsResponse(t, first)["next_cursor"].(string)
	mismatches := []string{
		"/api/v1/release-records?limit=1&environment=dev&cursor=",
		"/api/v1/release-records?limit=1&version=1.0.1&cursor=",
		"/api/v1/release-records?limit=1&gate_status=blocked&cursor=",
		"/api/v1/release-records?limit=1&batch_id=b-1&cursor=",
		"/api/v1/release-records?limit=1&recorded_from=2000-01-01&cursor=",
		"/api/v1/release-records?limit=1&recorded_to=2999-01-01&cursor=",
		"/api/v1/release-records?limit=1&cursor=",
	}
	for _, path := range mismatches {
		wantError(t, doRequest(t, router, http.MethodGet, path+cursor, ""),
			http.StatusBadRequest, store.CodeInvalidReleaseRecordQueryV1)
	}
	// Same normalized filters continue paging, even with a different limit.
	same := doRequest(t, router, http.MethodGet,
		"/api/v1/release-records?limit=5&environment=prod&cursor="+cursor, "")
	if same.Code != http.StatusOK {
		t.Fatalf("same filter cursor rejected: %d %s", same.Code, same.Body.String())
	}
}

func TestListRecordsRepeatedCursorIsStable(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "prod")
	for i := 0; i < 3; i++ {
		createRecordOK(t, router, withVersion(validRecordBody, "2.0."+string(rune('0'+i))))
	}
	first := doRequest(t, router, http.MethodGet, "/api/v1/release-records?limit=1", "")
	cursor := decodeRecordsResponse(t, first)["next_cursor"].(string)
	pageA := doRequest(t, router, http.MethodGet, "/api/v1/release-records?limit=1&cursor="+cursor, "")
	pageB := doRequest(t, router, http.MethodGet, "/api/v1/release-records?limit=1&cursor="+cursor, "")
	if pageA.Body.String() != pageB.Body.String() {
		t.Fatalf("same cursor must yield the same page:\n%s\n%s", pageA.Body.String(), pageB.Body.String())
	}
}

func TestListRecordsCursorDoesNotLeakInternalID(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "prod")
	createRecordOK(t, router, validRecordBody)
	cursor := decodeRecordsResponse(t, doRequest(t, router, http.MethodGet,
		"/api/v1/release-records?limit=1", ""))["next_cursor"].(string)
	payload, err := base64.RawURLEncoding.DecodeString(strings.Split(cursor, ".")[0])
	if err != nil {
		t.Fatalf("decode cursor payload: %v", err)
	}
	decoded := string(payload)
	if strings.Contains(decoded, `"p":`) || strings.Contains(decoded, `"record_id"`) {
		t.Fatalf("cursor payload leaks internal keyset: %s", decoded)
	}
}

func TestListRecordsSameSecondOrderAndPageBoundary(t *testing.T) {
	router, db := newChangeTestRouter(t)
	registerEnvironmentOK(t, router, "prod")
	var firstID string
	for i := 0; i < 3; i++ {
		version := "3.0." + string(rune('0'+i))
		record := createRecordOK(t, router, changeRecordBody("prod", version, "allowed", "rb", "", entryMap(1, "feature", "t", "d")))
		pinRecordedAt(t, db, "prod", version, "2026-10-01T10:00:00Z")
		if i == 0 {
			firstID = record["id"].(string)
		}
	}
	// Write order newest-first within the same second: latest insert first.
	page1 := doRequest(t, router, http.MethodGet, "/api/v1/release-records?limit=2", "")
	ids1 := recordIDs(t, decodeRecordsResponse(t, page1))
	if len(ids1) != 2 || ids1[1] == firstID {
		t.Fatalf("first page order = %v", ids1)
	}
	cursor := decodeRecordsResponse(t, page1)["next_cursor"].(string)
	page2 := doRequest(t, router, http.MethodGet, "/api/v1/release-records?limit=2&cursor="+cursor, "")
	ids2 := recordIDs(t, decodeRecordsResponse(t, page2))
	if len(ids2) != 1 || ids2[0] != firstID {
		t.Fatalf("second page = %v, want earliest insert %s", ids2, firstID)
	}
	if decodeRecordsResponse(t, page2)["next_cursor"] != "" {
		t.Fatal("last page next_cursor must be empty")
	}
}

func TestListRecordsCursorWalkIgnoresConcurrentInserts(t *testing.T) {
	router, db := newChangeTestRouter(t)
	registerEnvironmentOK(t, router, "prod")
	var want []string
	for i := 0; i < 3; i++ {
		version := "4.0." + string(rune('0'+i))
		record := createRecordOK(t, router, changeRecordBody("prod", version, "allowed", "rb", "", entryMap(1, "feature", "t", "d")))
		pinRecordedAt(t, db, "prod", version, "2026-10-01T10:00:0"+string(rune('0'+i))+"Z")
		want = append(want, record["id"].(string))
	}
	firstPage := doRequest(t, router, http.MethodGet, "/api/v1/release-records?limit=2", "")
	cursor := decodeRecordsResponse(t, firstPage)["next_cursor"].(string)

	// A newer record lands after the first request; it must not enter the walk.
	createRecordOK(t, router, changeRecordBody("prod", "4.9.9", "allowed", "rb", "", entryMap(1, "feature", "t", "d")))
	pinRecordedAt(t, db, "prod", "4.9.9", "2026-12-31T00:00:00Z")
	rest := doRequest(t, router, http.MethodGet,
		"/api/v1/release-records?limit=2&cursor="+cursor, "")
	restBody := decodeRecordsResponse(t, rest)
	ids := recordIDs(t, restBody)
	firstPageIDs := recordIDs(t, decodeRecordsResponse(t, firstPage))
	if len(ids) != 1 || ids[0] != want[0] {
		t.Fatalf("continuation = %v, want only the unseen oldest %s; first page %v", ids, want[0], firstPageIDs)
	}
	if restBody["next_cursor"] != "" {
		t.Fatal("walk must end without the concurrently inserted record")
	}
	newID := createRecordLookupByVersion(t, db, "4.9.9")
	for _, id := range append(firstPageIDs, ids...) {
		if id == newID {
			t.Fatal("concurrently inserted record leaked into the cursor walk")
		}
	}
	// A fresh first request surfaces the new record.
	fresh := doRequest(t, router, http.MethodGet, "/api/v1/release-records?limit=1", "")
	if ids := recordIDs(t, decodeRecordsResponse(t, fresh)); ids[0] != newID {
		t.Fatalf("fresh first request must include the new newest record, got %v", ids)
	}
}

func TestListRecordsKeepsExistingErrorSemantics(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "prod")
	createRecordOK(t, router, validRecordBody)
	wantError(t, doRequest(t, router, http.MethodGet, "/api/v1/release-records?environment=ghost", ""),
		http.StatusNotFound, store.CodeEnvironmentNotFoundV1)
	wantError(t, doRequest(t, router, http.MethodGet, "/api/v1/release-records?gate_status=nope", ""),
		http.StatusUnprocessableEntity, store.CodeReleaseValidationV1)
	wantError(t, doRequest(t, router, http.MethodGet, "/api/v1/release-records?recorded_from=tuesday", ""),
		http.StatusUnprocessableEntity, store.CodeReleaseValidationV1)
}

func TestListRecordsStorageUnavailable(t *testing.T) {
	router, db := newChangeTestRouter(t)
	registerEnvironmentOK(t, router, "prod")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	wantError(t, doRequest(t, router, http.MethodGet, "/api/v1/release-records", ""),
		http.StatusServiceUnavailable, "storage_unavailable")
}

func createRecordLookupByVersion(t *testing.T, db *store.Store, version string) string {
	t.Helper()
	records, err := db.ListReleaseRecords(store.ReleaseRecordFilter{Environment: "prod", Version: version})
	if err != nil || len(records) != 1 {
		t.Fatalf("lookup %s: %+v %v", version, records, err)
	}
	return records[0].PublicID
}
