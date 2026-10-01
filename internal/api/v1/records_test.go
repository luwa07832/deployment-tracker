package v1

import (
	"net/http"
	"strings"
	"testing"

	"github.com/luwa07832/deployment-tracker/internal/store"
)

func TestCreateReleaseRecordReturnsFullRecord(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "prod")
	record := createRecordOK(t, router, validRecordBody)

	if record["environment"] != "prod" || record["version"] != "1.2.0" {
		t.Fatalf("record = %v", record)
	}
	if record["gate_status"] != "allowed" || record["rollback_point"] != "snapshot:1.1.0" {
		t.Fatalf("record = %v", record)
	}
	changes := record["changes"].([]any)
	if len(changes) != 2 {
		t.Fatalf("changes = %v", changes)
	}
	first := changes[0].(map[string]any)
	if first["sequence"] != float64(1) || first["category"] != "feature" ||
		first["title"] != "add login" || first["description"] != "users can log in" {
		t.Fatalf("first change = %v", first)
	}
	id, _ := record["id"].(string)
	if !strings.HasPrefix(id, "rel_") || len(id) != len("rel_")+32 {
		t.Fatalf("id = %v, want an opaque rel_ identifier", record["id"])
	}
	if record["recorded_at"] == "" {
		t.Fatalf("recorded_at missing: %v", record)
	}
	if _, present := record["environment_id"]; present {
		t.Fatalf("response leaks internal storage fields: %v", record)
	}
}

func TestCreateRecordAssignsSequenceWhenOmitted(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "prod")
	body := `{"environment":"prod","version":"1.0.0","changes":[
		{"category":"feature","title":"a","description":"first"},
		{"category":"fix","title":"b","description":"second"}],
		"gate_status":"allowed","rollback_point":"0.9.0"}`
	record := createRecordOK(t, router, body)
	changes := record["changes"].([]any)
	if changes[0].(map[string]any)["sequence"] != float64(1) ||
		changes[1].(map[string]any)["sequence"] != float64(2) {
		t.Fatalf("auto sequence = %v", changes)
	}
}

func TestCreateRecordEnvironmentNotFound(t *testing.T) {
	router := newTestRouter(t)
	wantError(t, doRequest(t, router, http.MethodPost, "/api/v1/release-records", validRecordBody),
		http.StatusNotFound, store.CodeEnvironmentNotFoundV1)
}

func TestCreateRecordDuplicateConflict(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "prod")
	createRecordOK(t, router, validRecordBody)
	recorder := doRequest(t, router, http.MethodPost, "/api/v1/release-records", validRecordBody)
	wantError(t, recorder, http.StatusConflict, store.CodeReleaseAlreadyExistsV1)
}

func TestCreateRecordValidationFailures(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "prod")
	wrap := func(inner string) string {
		return `{"environment":"prod","version":"1.0.0","changes":[` + inner +
			`],"gate_status":"allowed","rollback_point":"0.9.0"}`
	}
	invalid := []string{
		`{"version":"1.0.0","changes":[],"gate_status":"allowed","rollback_point":"0.9.0"}`,
		`{"environment":"prod","changes":[],"gate_status":"allowed","rollback_point":"0.9.0"}`,
		`{"environment":"prod","version":"1.0.0","gate_status":"allowed","rollback_point":"0.9.0"}`,
		`{"environment":"prod","version":"1.0.0","changes":[],"rollback_point":"0.9.0"}`,
		`{"environment":"prod","version":"1.0.0","changes":[],"gate_status":"allowed"}`,
		`{"environment":"prod","version":"1.0.0","changes":[],"gate_status":"shipped","rollback_point":"0.9.0"}`,
		`{"environment":"prod","version":"1.0.0","changes":[],"gate_status":"allowed","rollback_point":"  "}`,
		wrap(`{"sequence":1,"category":"feature","title":"a"}`),
		wrap(`{"sequence":1,"category":"feature","description":"d"}`),
		wrap(`{"sequence":1,"title":"a","description":"d"}`),
		wrap(`{"sequence":0,"category":"feature","title":"a","description":"d"}`),
		wrap(`{"sequence":1,"category":"feature","title":"a","description":"d"},{"sequence":1,"category":"fix","title":"b","description":"e"}`),
		wrap(`{"sequence":1,"category":"feature","title":"a","description":"d"},{"category":"fix","title":"b","description":"e"}`),
		`{"environment":"prod","version":"1.0.0","changes":"nope","gate_status":"allowed","rollback_point":"0.9.0"}`,
	}
	for _, body := range invalid {
		wantError(t, doRequest(t, router, http.MethodPost, "/api/v1/release-records", body),
			http.StatusUnprocessableEntity, store.CodeReleaseValidationV1)
	}
	wantError(t, doRequest(t, router, http.MethodPost, "/api/v1/release-records", `{not json`),
		http.StatusBadRequest, store.CodeInvalidRequest)
}

func TestNewRecordImmediatelyQueryable(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "prod")
	created := createRecordOK(t, router, validRecordBody)
	id := created["id"].(string)

	recorder := doRequest(t, router, http.MethodGet, "/api/v1/release-records/"+id, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("get by id status = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	got := decodeBody(t, recorder)["release_record"].(map[string]any)
	if got["id"] != id || len(got["changes"].([]any)) != 2 || got["rollback_point"] != "snapshot:1.1.0" {
		t.Fatalf("got = %v", got)
	}

	recorder = doRequest(t, router, http.MethodGet, "/api/v1/release-records?environment=prod&version=1.2.0&gate_status=allowed", "")
	list := decodeBody(t, recorder)["release_records"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["id"] != id {
		t.Fatalf("filtered list = %v", list)
	}
}

func TestGetRecordNotFound(t *testing.T) {
	router := newTestRouter(t)
	wantError(t, doRequest(t, router, http.MethodGet, "/api/v1/release-records/rel_missing", ""),
		http.StatusNotFound, store.CodeReleaseRecordNotFoundV1)
}

func TestListRecordsOrdersNewestFirstAndValidates(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "prod")
	registerEnvironmentOK(t, router, "staging")
	first := createRecordOK(t, router, withVersion(validRecordBody, "1.0.0"))
	second := createRecordOK(t, router, strings.ReplaceAll(validRecordBody, `"prod"`, `"staging"`))
	third := createRecordOK(t, router, validRecordBody)

	recorder := doRequest(t, router, http.MethodGet, "/api/v1/release-records", "")
	list := decodeBody(t, recorder)["release_records"].([]any)
	got := []string{
		list[0].(map[string]any)["id"].(string),
		list[1].(map[string]any)["id"].(string),
		list[2].(map[string]any)["id"].(string),
	}
	want := []string{third["id"].(string), second["id"].(string), first["id"].(string)}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want newest first %v", got, want)
	}
	if !strings.Contains(recorder.Body.String(), `"release_records":[`) {
		t.Fatalf("empty list must serialize as an array: %s", recorder.Body.String())
	}

	wantError(t, doRequest(t, router, http.MethodGet, "/api/v1/release-records?environment=ghost", ""),
		http.StatusNotFound, store.CodeEnvironmentNotFoundV1)
	wantError(t, doRequest(t, router, http.MethodGet, "/api/v1/release-records?gate_status=nope", ""),
		http.StatusUnprocessableEntity, store.CodeReleaseValidationV1)
	wantError(t, doRequest(t, router, http.MethodGet, "/api/v1/release-records?recorded_from=tuesday", ""),
		http.StatusUnprocessableEntity, store.CodeReleaseValidationV1)

	recorder = doRequest(t, router, http.MethodGet, "/api/v1/release-records?recorded_to=2000-01-01", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("date bound status = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	if list := decodeBody(t, recorder)["release_records"].([]any); len(list) != 0 {
		t.Fatalf("past bound = %v, want no records", list)
	}
	recorder = doRequest(t, router, http.MethodGet, "/api/v1/release-records?recorded_from=2999-01-01T00:00:00Z", "")
	if list := decodeBody(t, recorder)["release_records"].([]any); len(list) != 0 {
		t.Fatalf("future bound = %v, want no records", list)
	}
}

func withVersion(body, version string) string {
	return strings.ReplaceAll(body, `"version": "1.2.0"`, `"version": "`+version+`"`)
}
