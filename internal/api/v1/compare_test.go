package v1

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/luwa07832/deployment-tracker/internal/store"
)

func recordBody(environment, version, gate, rollback, entries string) string {
	return `{"environment":"` + environment + `","version":"` + version + `",
		"changes":[` + entries + `],
		"gate_status":"` + gate + `","rollback_point":"` + rollback + `"}`
}

func entry(seq int, category, title, description string) string {
	return `{"sequence":` + itoa(seq) + `,"category":"` + category + `","title":"` + title +
		`","description":"` + description + `"}`
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	digits := ""
	for value > 0 {
		digits = string(rune('0'+value%10)) + digits
		value /= 10
	}
	return digits
}

func TestCompareEnvironmentsFullDiff(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "staging")
	registerEnvironmentOK(t, router, "prod")
	createRecordOK(t, router, recordBody("staging", "1.0.0", "allowed", "0.9.0",
		entry(1, "feature", "a", "a desc")+","+entry(2, "fix", "b", "b desc")))
	createRecordOK(t, router, recordBody("staging", "1.1.0", "pending", "1.0.0",
		entry(1, "feature", "a", "a desc")))
	createRecordOK(t, router, recordBody("prod", "1.0.0", "blocked", "0.8.0",
		entry(1, "feature", "a", "a desc")+","+entry(2, "feature", "d", "d desc")))
	createRecordOK(t, router, recordBody("prod", "1.2.0", "allowed", "1.1.0",
		entry(1, "feature", "e", "e desc")))

	recorder := doRequest(t, router, http.MethodGet, "/api/v1/compare?left=staging&right=prod", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	body := decodeBody(t, recorder)
	if body["left"] != "staging" || body["right"] != "prod" {
		t.Fatalf("sides = %v/%v", body["left"], body["right"])
	}
	if body["compared_at"] == "" {
		t.Fatal("compared_at missing")
	}
	if _, present := body["baseline_version"]; present {
		t.Fatalf("unbounded compare must omit baseline_version: %v", body)
	}
	for key, want := range map[string]string{
		"left_versions":       `["1.0.0","1.1.0"]`,
		"right_versions":      `["1.0.0","1.2.0"]`,
		"common_versions":     `["1.0.0"]`,
		"only_left_versions":  `["1.1.0"]`,
		"only_right_versions": `["1.2.0"]`,
	} {
		if !strings.Contains(recorder.Body.String(), `"`+key+`":`+want) {
			t.Fatalf("body missing %s=%s\n%s", key, want, recorder.Body.String())
		}
	}
	diffs := body["version_diffs"].([]any)
	if len(diffs) != 1 {
		t.Fatalf("version_diffs = %v, want 1", diffs)
	}
	diff := diffs[0].(map[string]any)
	gate := diff["gate_status"].(map[string]any)
	if gate["left"] != "allowed" || gate["right"] != "blocked" || gate["changed"] != true {
		t.Fatalf("gate diff = %v", gate)
	}
	fields := diff["field_diffs"].([]any)
	if len(fields) != 1 || fields[0].(map[string]any)["field"] != "rollback_point" {
		t.Fatalf("field diffs = %v, want rollback_point", fields)
	}
	added := diff["added_changes"].([]any)
	if len(added) != 1 || added[0].(map[string]any)["title"] != "d" {
		t.Fatalf("added = %v, want [d]", added)
	}
	removed := diff["removed_changes"].([]any)
	if len(removed) != 1 || removed[0].(map[string]any)["title"] != "b" {
		t.Fatalf("removed = %v, want [b]", removed)
	}
}

func TestCompareDetectsChangedChangeEntries(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "a")
	registerEnvironmentOK(t, router, "b")
	createRecordOK(t, router, recordBody("a", "1.0.0", "allowed", "0.9.0",
		entry(1, "feature", "x", "old description")))
	createRecordOK(t, router, recordBody("b", "1.0.0", "allowed", "0.9.0",
		entry(2, "fix", "x", "new description")))

	body := decodeBody(t, doRequest(t, router, http.MethodGet, "/api/v1/compare?left=a&right=b", ""))
	diff := body["version_diffs"].([]any)[0].(map[string]any)
	changed := diff["changed_changes"].([]any)
	if len(changed) != 1 {
		t.Fatalf("changed = %v, want 1", changed)
	}
	pair := changed[0].(map[string]any)
	if pair["left"].(map[string]any)["category"] != "feature" ||
		pair["right"].(map[string]any)["category"] != "fix" {
		t.Fatalf("changed pair = %v", pair)
	}
	gate := diff["gate_status"].(map[string]any)
	if gate["changed"] != false {
		t.Fatalf("gate = %v, want unchanged", gate)
	}
}

func TestCompareVersionAndTimeRanges(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "a")
	registerEnvironmentOK(t, router, "b")
	first := createRecordOK(t, router, recordBody("a", "1.0.0", "allowed", "0.9.0", entry(1, "feature", "x", "d")))
	time.Sleep(1100 * time.Millisecond)
	createRecordOK(t, router, recordBody("a", "2.0.0", "allowed", "1.0.0", entry(1, "feature", "z", "d")))
	createRecordOK(t, router, recordBody("b", "1.5.0", "allowed", "1.0.0", entry(1, "feature", "y", "d")))

	// Cutoff time inclusive: only the record registered at or before the
	// first record's instant participates.
	cutoff := first["recorded_at"].(string)
	recorder := doRequest(t, router, http.MethodGet,
		"/api/v1/compare?left=a&right=b&as_of="+url.QueryEscape(cutoff), "")
	body := decodeBody(t, recorder)
	if body["as_of"] != cutoff {
		t.Fatalf("as_of = %v, want %s echoed", body["as_of"], cutoff)
	}
	raw := recorder.Body.String()
	for _, want := range []string{
		`"left_versions":["1.0.0"]`,
		`"right_versions":[]`,
		`"common_versions":[]`,
		`"only_left_versions":["1.0.0"]`,
		`"only_right_versions":[]`,
		`"version_diffs":[]`,
	} {
		if !strings.Contains(raw, want) {
			t.Fatalf("body missing %s\n%s", want, raw)
		}
	}

	// A date cutoff in the past legitimately contains nothing: it is a range,
	// not a named version, so it stays a 200 empty result.
	recorder = doRequest(t, router, http.MethodGet, "/api/v1/compare?left=a&right=b&as_of=2000-01-01", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("past as_of status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}

	// Exact version mode keeps that version on each side even when only one
	// side has it, and reports the baseline used.
	recorder = doRequest(t, router, http.MethodGet, "/api/v1/compare?left=a&right=b&version=2.0.0", "")
	body = decodeBody(t, recorder)
	if body["baseline_version"] != "2.0.0" {
		t.Fatalf("baseline = %v, want 2.0.0", body["baseline_version"])
	}
	leftVersions := body["left_versions"].([]any)
	rightVersions := body["right_versions"].([]any)
	if len(leftVersions) != 1 || len(rightVersions) != 0 {
		t.Fatalf("exact mode sides = %v / %v", leftVersions, rightVersions)
	}
}

func TestCompareErrors(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "a")
	createRecordOK(t, router, recordBody("a", "1.0.0", "allowed", "0.9.0", entry(1, "feature", "x", "d")))

	cases := []struct {
		target string
		status int
		code   string
	}{
		{"/api/v1/compare?left=a&right=a", http.StatusBadRequest, store.CodeSameEnvironmentCompareV1},
		{"/api/v1/compare?left=a", http.StatusBadRequest, store.CodeInvalidCompareRangeV1},
		{"/api/v1/compare?left=a&right=b&version=1.0.0&as_of=2026-01-01", http.StatusBadRequest, store.CodeInvalidCompareRangeV1},
		{"/api/v1/compare?left=a&right=b&as_of=tuesday", http.StatusBadRequest, store.CodeInvalidCompareRangeV1},
		{"/api/v1/compare?left=a&right=ghost", http.StatusNotFound, store.CodeEnvironmentNotFoundV1},
		{"/api/v1/compare?left=ghost&right=a", http.StatusNotFound, store.CodeEnvironmentNotFoundV1},
		{"/api/v1/compare?left=a&right=ghost&version=1.0.0", http.StatusNotFound, store.CodeEnvironmentNotFoundV1},
	}
	for _, tc := range cases {
		wantError(t, doRequest(t, router, http.MethodGet, tc.target, ""), tc.status, tc.code)
	}

	// Same-environment is reported before the environment existence check.
	wantError(t, doRequest(t, router, http.MethodGet, "/api/v1/compare?left=ghost&right=ghost", ""),
		http.StatusBadRequest, store.CodeSameEnvironmentCompareV1)

	// Version existing on neither side must not collapse into an empty diff.
	registerEnvironmentOK(t, router, "b")
	wantError(t, doRequest(t, router, http.MethodGet, "/api/v1/compare?left=a&right=b&version=9.9.9", ""),
		http.StatusBadRequest, store.CodeReleaseVersionNotFoundV1)

}

func TestCompareIsDeterministicAcrossReads(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "a")
	registerEnvironmentOK(t, router, "b")
	createRecordOK(t, router, recordBody("a", "1.0.0", "allowed", "0.9.0", entry(1, "feature", "x", "d")))
	createRecordOK(t, router, recordBody("b", "1.0.0", "blocked", "0.8.0", entry(1, "feature", "x", "d")))

	first := doRequest(t, router, http.MethodGet, "/api/v1/compare?left=a&right=b", "")
	second := doRequest(t, router, http.MethodGet, "/api/v1/compare?left=a&right=b", "")
	stripTimestamp := func(raw string) string {
		return strings.SplitN(raw, `"compared_at"`, 2)[0]
	}
	if stripTimestamp(first.Body.String()) != stripTimestamp(second.Body.String()) {
		t.Fatalf("non-deterministic comparison:\n%s\n%s", first.Body.String(), second.Body.String())
	}
}
