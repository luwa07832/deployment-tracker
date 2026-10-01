package api

import (
	"database/sql"
	"net/http"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// TestReleaseHistoryNewestFirstAndFields returns the traced facts per entry.
func TestReleaseHistoryNewestFirstAndFields(t *testing.T) {
	router, _ := newReleaseTestRouter(t)
	createReleaseOK(t, router, releaseBody("prod", "1.0.0", "allowed", "0.9.0", "a"))
	createReleaseOK(t, router, releaseBody("prod", "1.1.0", "pending", "1.0.0", "a", "b"))
	createReleaseOK(t, router, releaseBody("prod", "1.2.0", "blocked", "1.1.0", "a", "b", "c"))
	recorder := doRequest(t, router, http.MethodGet, "/environments/prod/release-history", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	body := decodeBody(t, recorder)
	if body["environment"] != "prod" {
		t.Fatalf("environment = %v", body["environment"])
	}
	releases := body["releases"].([]any)
	if len(releases) != 3 {
		t.Fatalf("releases len = %d, want 3", len(releases))
	}
	versions := []string{}
	for _, item := range releases {
		entry, _ := item.(map[string]any)
		versions = append(versions, entry["version"].(string))
		if entry["gate_status"] == nil || entry["rollback_point"] == nil || entry["changes"] == nil {
			t.Fatalf("history entry must retain gate_status, rollback_point and changes: %v", entry)
		}
	}
	if versions[0] != "1.2.0" || versions[1] != "1.1.0" || versions[2] != "1.0.0" {
		t.Fatalf("history order = %v, want newest first 1.2.0,1.1.0,1.0.0", versions)
	}
	if _, present := body["next_cursor"]; present {
		t.Fatalf("next_cursor must be omitted when the page covers everything: %s", recorder.Body.String())
	}
}

// TestReleaseHistoryPagingIsStable walks every page and verifies there are no
// repeats or gaps, and that later pages expose the traceback facts too.
func TestReleaseHistoryPagingIsStable(t *testing.T) {
	router, _ := newReleaseTestRouter(t)
	const total = 7
	for i := 0; i < total; i++ {
		createReleaseOK(t, router, releaseBody("prod", "1.0."+string(rune('0'+i)), "allowed", "0.9.0", "c"))
	}
	seen := map[float64]bool{}
	collected := []string{}
	cursor := ""
	pageCount := 0
	for {
		target := "/environments/prod/release-history?limit=3"
		if cursor != "" {
			target += "&cursor=" + cursor
		}
		recorder := doRequest(t, router, http.MethodGet, target, "")
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
		}
		body := decodeBody(t, recorder)
		releases, _ := body["releases"].([]any)
		if len(releases) == 0 {
			t.Fatalf("empty page %d", pageCount)
		}
		for _, item := range releases {
			entry, _ := item.(map[string]any)
			id := entry["id"]
			if seen[id.(float64)] {
				t.Fatalf("release %v repeated across pages", id)
			}
			seen[id.(float64)] = true
			collected = append(collected, entry["version"].(string))
		}
		next, hasNext := body["next_cursor"].(string)
		pageCount++
		if !hasNext {
			break
		}
		cursor = next
	}
	if len(seen) != total {
		t.Fatalf("collected %d distinct releases, want %d (%v)", len(seen), total, collected)
	}
	if pageCount != 3 {
		t.Fatalf("page count = %d, want 3", pageCount)
	}
	// Pages are newest first overall; versions were inserted 1.0.0..1.0.6.
	if collected[0] != "1.0.6" || collected[len(collected)-1] != "1.0.0" {
		t.Fatalf("paged order = %v, want newest first", collected)
	}
}

// TestReleaseHistoryPagingStableAcrossWrites keeps a cursor valid after a new
// release is inserted: keyset paging resumes after the stored position
// without repeating or skipping the earlier releases.
func TestReleaseHistoryPagingStableAcrossWrites(t *testing.T) {
	router, _ := newReleaseTestRouter(t)
	createReleaseOK(t, router, releaseBody("prod", "1.0.0", "allowed", "0.9.0", "a"))
	createReleaseOK(t, router, releaseBody("prod", "1.0.1", "allowed", "0.9.0", "a"))
	createReleaseOK(t, router, releaseBody("prod", "1.0.2", "allowed", "0.9.0", "a"))
	first := doRequest(t, router, http.MethodGet, "/environments/prod/release-history?limit=2", "")
	firstBody := decodeBody(t, first)
	cursor, _ := firstBody["next_cursor"].(string)
	if cursor == "" {
		t.Fatalf("expected a next cursor (body %s)", first.Body.String())
	}
	// A newer release lands after the first page was read.
	createReleaseOK(t, router, releaseBody("prod", "1.0.3", "allowed", "0.9.0", "a"))
	second := doRequest(t, router, http.MethodGet, "/environments/prod/release-history?limit=2&cursor="+cursor, "")
	secondBody := decodeBody(t, second)
	releases, _ := secondBody["releases"].([]any)
	versions := []string{}
	for _, item := range releases {
		versions = append(versions, item.(map[string]any)["version"].(string))
	}
	if len(versions) != 1 || versions[0] != "1.0.0" {
		t.Fatalf("second page = %v, want only the older 1.0.0 row", versions)
	}
	if _, present := secondBody["next_cursor"]; present {
		t.Fatalf("second page must be the last page: %s", second.Body.String())
	}
}

// TestReleaseHistorySameTimestampTieBreak seeds rows with an identical
// created_at and verifies both orders and pages are still deterministic.
func TestReleaseHistorySameTimestampTieBreak(t *testing.T) {
	router, path := newReleaseTestRouter(t)
	raw, err := sql.Open("sqlite", filepath.Join(path))
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer raw.Close()
	for i := 0; i < 4; i++ {
		version := "1.0." + string(rune('0'+i))
		if _, err := raw.Exec(
			`INSERT INTO deployments (name, environment, version, changes_json, gate_status, rollback_point, created_at)
			 VALUES ('', 'prod', ?, '["c"]', 'allowed', '0.9.0', '2026-10-01T08:00:00Z')`,
			version,
		); err != nil {
			t.Fatalf("seed same-timestamp row: %v", err)
		}
	}
	seen := map[int64]bool{}
	cursor := ""
	for {
		target := "/environments/prod/release-history?limit=2"
		if cursor != "" {
			target += "&cursor=" + cursor
		}
		recorder := doRequest(t, router, http.MethodGet, target, "")
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
		}
		body := decodeBody(t, recorder)
		for _, item := range body["releases"].([]any) {
			id := int64(item.(map[string]any)["id"].(float64))
			if seen[id] {
				t.Fatalf("id %d repeated across same-timestamp pages", id)
			}
			seen[id] = true
		}
		next, ok := body["next_cursor"].(string)
		if !ok {
			break
		}
		cursor = next

	}
	if len(seen) != 4 {
		t.Fatalf("paged %d distinct same-timestamp rows, want 4", len(seen))
	}
}

func TestReleaseHistoryEnvironmentNotFound(t *testing.T) {
	router, _ := newReleaseTestRouter(t)
	recorder := doRequest(t, router, http.MethodGet, "/environments/ghost/release-history", "")
	wantError(t, recorder, http.StatusNotFound, "environment_not_found")
}

func TestReleaseHistoryBadLimitAndCursor(t *testing.T) {
	router, _ := newReleaseTestRouter(t)
	createReleaseOK(t, router, releaseBody("prod", "1.0.0", "allowed", "0.9.0", "a"))
	for _, target := range []string{
		"/environments/prod/release-history?limit=0",
		"/environments/prod/release-history?limit=101",
		"/environments/prod/release-history?limit=abc",
		"/environments/prod/release-history?cursor=not-base64!!",
	} {
		recorder := doRequest(t, router, http.MethodGet, target, "")
		wantError(t, recorder, http.StatusBadRequest, "invalid_request")
	}
}
