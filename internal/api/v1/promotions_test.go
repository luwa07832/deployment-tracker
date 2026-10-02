package v1

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"testing"

	"github.com/luwa07832/deployment-tracker/internal/store"
)

// batchRecordBody builds a POST body for a release record belonging to a
// batch. changeTitles become single-entry change lists.
func batchRecordBody(batchID, environment, version, gate, rollback string, changeTitles ...string) string {
	var changes []string
	for i, title := range changeTitles {
		changes = append(changes, fmt.Sprintf(
			`{"sequence":%d,"category":"feature","title":"%s","description":"desc %s"}`, i+1, title, title))
	}
	return fmt.Sprintf(`{"environment":"%s","version":"%s","batch_id":"%s","changes":[%s],
"gate_status":"%s","rollback_point":"%s"}`, environment, version, batchID, strings.Join(changes, ","), gate, rollback)
}

func TestPromotionChainReturnsNodesSegmentsAndConsistency(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "dev")
	registerEnvironmentOK(t, router, "test")
	registerEnvironmentOK(t, router, "staging")
	registerEnvironmentOK(t, router, "prod")

	// batch-7: identical facts everywhere -> fully consistent chain.
	batch := "batch-7"
	for _, env := range []string{"dev", "test", "staging", "prod"} {
		createRecordOK(t, router, batchRecordBody(batch, env, "1.0.0", "allowed", "rb:0.9.0", "add login"))
	}
	recorder := doRequest(t, router, http.MethodGet,
		"/api/v1/release-batches/"+batch+"/promotion-chain?environments=dev,test,staging,prod", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("chain status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	body := decodeBody(t, recorder)
	if body["batch_id"] != batch {
		t.Fatalf("batch_id = %v", body["batch_id"])
	}
	envs := body["environments"].([]any)
	if len(envs) != 4 || envs[0] != "dev" || envs[3] != "prod" {
		t.Fatalf("environments = %v", envs)
	}
	nodes := body["nodes"].([]any)
	if len(nodes) != 4 {
		t.Fatalf("nodes = %v", nodes)
	}
	first := nodes[0].(map[string]any)
	if first["environment"] != "dev" {
		t.Fatalf("first node = %v", first)
	}
	releases := first["releases"].([]any)
	if len(releases) != 1 {
		t.Fatalf("releases = %v", releases)
	}
	effective := first["effective_release"].(map[string]any)
	if effective["version"] != "1.0.0" || effective["gate_status"] != "allowed" ||
		effective["rollback_point"] != "rb:0.9.0" || effective["batch_id"] != nil {
		t.Fatalf("effective fact = %v", effective)
	}
	if titlesOf(effective) != "add login" {
		t.Fatalf("effective changes = %v", effective["changes"])
	}
	segments := body["segment_diffs"].([]any)
	if len(segments) != 0 {
		t.Fatalf("segment_diffs = %v, want empty array", segments)
	}
	if body["consistent"] != true {
		t.Fatalf("consistent = %v, want true", body["consistent"])
	}
}

func titlesOf(record map[string]any) string {
	changes := record["changes"].([]any)
	var titles []string
	for _, raw := range changes {
		titles = append(titles, raw.(map[string]any)["title"].(string))
	}
	return strings.Join(titles, ",")
}

func TestPromotionChainDetectsEveryDifferenceCategory(t *testing.T) {
	router := newTestRouter(t)
	for _, env := range []string{"dev", "test", "staging", "prod"} {
		registerEnvironmentOK(t, router, env)
	}
	batch := "batch-diff"
	// dev -> test: change "new in test" is added, "dropped later" is missing,
	// version/gate/rollback all agree.
	createRecordOK(t, router, batchRecordBody(batch, "dev", "1.0.0", "allowed", "rb:0.9.0",
		"dropped later", "inconsistent entry"))
	createRecordOK(t, router, `{"environment":"test","version":"1.0.0","batch_id":"batch-diff","changes":[
		{"sequence":1,"category":"feature","title":"new in test","description":"added downstream"},
		{"sequence":2,"category":"feature","title":"inconsistent entry","description":"description changed"}],
		"gate_status":"allowed","rollback_point":"rb:0.9.0"}`)
	// test -> staging: only gate status conflicts.
	createRecordOK(t, router, batchRecordBody(batch, "staging", "1.0.0", "blocked", "rb:0.9.0",
		"new in test", "inconsistent entry"))
	// staging -> prod: version divergence and rollback point mismatch.
	createRecordOK(t, router, batchRecordBody(batch, "prod", "1.0.1", "blocked", "rb:1.0.0",
		"new in test", "inconsistent entry"))

	recorder := doRequest(t, router, http.MethodGet,
		"/api/v1/release-batches/"+batch+"/promotion-chain?environments=dev,test,staging,prod", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	body := decodeBody(t, recorder)
	if body["consistent"] != false {
		t.Fatalf("consistent = %v, want false", body["consistent"])
	}
	segments := body["segment_diffs"].([]any)
	if len(segments) != 3 {
		t.Fatalf("segment count = %d, want 3: %v", len(segments), segments)
	}
	first := segments[0].(map[string]any)
	if first["from_environment"] != "dev" || first["to_environment"] != "test" {
		t.Fatalf("first segment = %v", first)
	}
	added := first["added_changes"].([]any)
	if len(added) != 1 || added[0].(map[string]any)["title"] != "new in test" {
		t.Fatalf("added = %v", added)
	}
	missing := first["missing_changes"].([]any)
	if len(missing) != 1 || missing[0].(map[string]any)["title"] != "dropped later" {
		t.Fatalf("missing = %v", missing)
	}
	inconsistent := first["inconsistent_changes"].([]any)
	if len(inconsistent) != 1 ||
		inconsistent[0].(map[string]any)["left"].(map[string]any)["description"] != "desc inconsistent entry" ||
		inconsistent[0].(map[string]any)["right"].(map[string]any)["description"] != "description changed" {
		t.Fatalf("inconsistent = %v", inconsistent)
	}
	if first["version"].(map[string]any)["changed"] != false ||
		first["gate_status"].(map[string]any)["changed"] != false ||
		first["rollback_point"].(map[string]any)["changed"] != false {
		t.Fatalf("first segment scalar conflicts = %v", first)
	}
	second := segments[1].(map[string]any)
	if second["gate_status"].(map[string]any)["changed"] != true ||
		len(second["added_changes"].([]any)) != 0 ||
		len(second["missing_changes"].([]any)) != 0 {
		t.Fatalf("gate-only segment = %v", second)
	}
	third := segments[2].(map[string]any)
	if third["version"].(map[string]any)["changed"] != true ||
		third["version"].(map[string]any)["left"] != "1.0.0" ||
		third["rollback_point"].(map[string]any)["changed"] != true {
		t.Fatalf("version/rollback segment = %v", third)
	}
}

func TestPromotionChainListsEveryReleaseAtOneNode(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "dev")
	registerEnvironmentOK(t, router, "test")
	batch := "batch-multi"
	createRecordOK(t, router, batchRecordBody(batch, "dev", "1.0.0", "pending", "rb:0.9.0", "a"))
	createRecordOK(t, router, batchRecordBody(batch, "dev", "1.0.1", "allowed", "rb:1.0.0", "a", "b"))
	createRecordOK(t, router, batchRecordBody(batch, "test", "1.0.1", "allowed", "rb:1.0.0", "a", "b"))

	body := decodeBody(t, doRequest(t, router, http.MethodGet,
		"/api/v1/release-batches/"+batch+"/promotion-chain?environments=dev,test", ""))
	nodes := body["nodes"].([]any)
	devReleases := nodes[0].(map[string]any)["releases"].([]any)
	if len(devReleases) != 2 {
		t.Fatalf("dev releases = %v, want both releases listed", devReleases)
	}
	if devReleases[0].(map[string]any)["version"] != "1.0.0" ||
		devReleases[1].(map[string]any)["version"] != "1.0.1" {
		t.Fatalf("dev releases must be chronological: %v", devReleases)
	}
	effective := nodes[0].(map[string]any)["effective_release"].(map[string]any)
	if effective["version"] != "1.0.1" {
		t.Fatalf("effective = %v, want latest release", effective)
	}
}

func TestBatchesSharingEnvironmentVersionAlwaysConflict(t *testing.T) {
	router := newTestRouter(t)
	for _, env := range []string{"dev", "test", "prod"} {
		registerEnvironmentOK(t, router, env)
	}
	created := createRecordOK(t, router, batchRecordBody("b-a", "dev", "9.9.9", "allowed", "rb:0.9.0", "shared"))

	// Same env+version in a different batch, with different gate and changes,
	// still conflicts: the (environment, version) pair is globally unique.
	wantError(t, doRequest(t, router, http.MethodPost, "/api/v1/release-records",
		batchRecordBody("b-b", "dev", "9.9.9", "blocked", "rb:0.8.0", "different")),
		http.StatusConflict, store.CodeReleaseAlreadyExistsV1)
	// Same batch duplicate submission conflicts as before.
	wantError(t, doRequest(t, router, http.MethodPost, "/api/v1/release-records",
		batchRecordBody("b-a", "dev", "9.9.9", "allowed", "rb:0.9.0", "shared")),
		http.StatusConflict, store.CodeReleaseAlreadyExistsV1)
	// Same env+version without a batch conflicts with the batched record.
	unbatched := `{"environment":"dev","version":"9.9.9","changes":[
		{"category":"feature","title":"shared","description":"desc shared"}],
		"gate_status":"allowed","rollback_point":"rb:0.9.0"}`
	wantError(t, doRequest(t, router, http.MethodPost, "/api/v1/release-records", unbatched),
		http.StatusConflict, store.CodeReleaseAlreadyExistsV1)

	// Different environments keep using the same version across both batches.
	createRecordOK(t, router, batchRecordBody("b-a", "prod", "9.9.9", "allowed", "rb:0.9.0", "shared"))
	createRecordOK(t, router, batchRecordBody("b-b", "test", "9.9.9", "allowed", "rb:0.9.0", "shared"))

	// The rejected b-b dev submission left no row, while the winner stays
	// queryable through the batch filter and the get endpoint.
	listBody := decodeBody(t, doRequest(t, router, http.MethodGet,
		"/api/v1/release-records?batch_id=b-a", ""))
	records := listBody["release_records"].([]any)
	if len(records) != 2 {
		t.Fatalf("b-a records = %v, want dev and prod", records)
	}
	devRecords := decodeBody(t, doRequest(t, router, http.MethodGet,
		"/api/v1/release-records?batch_id=b-b&environment=dev", ""))["release_records"].([]any)
	if len(devRecords) != 0 {
		t.Fatalf("conflicted b-b dev row survived: %v", devRecords)
	}
	got := decodeBody(t, doRequest(t, router, http.MethodGet,
		"/api/v1/release-records/"+created["id"].(string), ""))["release_record"].(map[string]any)
	if got["batch_id"] != "b-a" || got["gate_status"] != "allowed" {
		t.Fatalf("winning record = %v", got)
	}
	if len(got["changes"].([]any)) != 1 {
		t.Fatalf("winning record changes = %v", got["changes"])
	}

	// No second parallel effective version is visible to comparison reads.
	allDev := decodeBody(t, doRequest(t, router, http.MethodGet,
		"/api/v1/release-records?environment=dev&version=9.9.9", ""))["release_records"].([]any)
	if len(allDev) != 1 || allDev[0].(map[string]any)["id"] != created["id"] {
		t.Fatalf("parallel effective versions visible: %v", allDev)
	}
}

func seedPromotionFixture(t *testing.T, router *gin.Engine, batch string) {
	t.Helper()
	for _, env := range []string{"dev", "test", "staging", "prod"} {
		registerEnvironmentOK(t, router, env)
	}
	createRecordOK(t, router, batchRecordBody(batch, "dev", "1.0.0", "allowed", "rb:0.9.0",
		"alpha", "beta"))
	createRecordOK(t, router, batchRecordBody(batch, "test", "1.0.0", "allowed", "rb:0.9.0",
		"alpha", "beta"))
	createRecordOK(t, router, batchRecordBody(batch, "staging", "1.0.0", "allowed", "rb:0.9.0",
		"alpha"))
	createRecordOK(t, router, batchRecordBody(batch, "prod", "1.0.0", "allowed", "rb:0.9.0",
		"alpha"))
}

func chainPath(batch string, environments ...string) string {
	return "/api/v1/release-batches/" + batch +
		"/promotion-chain?environments=" + strings.Join(environments, ",")
}

func TestPromotionChainErrors(t *testing.T) {
	router := newTestRouter(t)
	seedPromotionFixture(t, router, "b-err")

	// Unknown batch is the single observable NotFound.
	wantError(t, doRequest(t, router, http.MethodGet, chainPath("ghost", "dev", "test"), ""),
		http.StatusNotFound, store.CodeReleaseBatchNotFoundV1)

	// Repeated environments: BadRequest naming the conflicting environment.
	recorder := doRequest(t, router, http.MethodGet, chainPath("b-err", "dev", "test", "dev"), "")
	wantError(t, recorder, http.StatusBadRequest, store.CodePromotionSequenceInvalidV1)
	if !strings.Contains(recorder.Body.String(), "dev") {
		t.Fatalf("duplicate message must name the environment: %s", recorder.Body.String())
	}

	// Unknown environment: BadRequest naming it.
	recorder = doRequest(t, router, http.MethodGet, chainPath("b-err", "dev", "qa"), "")
	wantError(t, recorder, http.StatusBadRequest, store.CodePromotionSequenceInvalidV1)
	if !strings.Contains(recorder.Body.String(), "qa") {
		t.Fatalf("unknown message must name the environment: %s", recorder.Body.String())
	}

	// Missing environments parameter.
	wantError(t, doRequest(t, router, http.MethodGet,
		"/api/v1/release-batches/b-err/promotion-chain", ""),
		http.StatusBadRequest, store.CodePromotionSequenceInvalidV1)

	// Empty segment in the sequence.
	wantError(t, doRequest(t, router, http.MethodGet,
		"/api/v1/release-batches/b-err/promotion-chain?environments=dev,,test", ""),
		http.StatusBadRequest, store.CodePromotionSequenceInvalidV1)

	// A registered environment without any release fact in this batch makes
	// the promotion order undeterminable: Conflict naming the node.
	registerEnvironmentOK(t, router, "qa")
	recorder = doRequest(t, router, http.MethodGet, chainPath("b-err", "dev", "qa"), "")
	wantError(t, recorder, http.StatusConflict, store.CodePromotionOrderConflictV1)
	if !strings.Contains(recorder.Body.String(), "qa") {
		t.Fatalf("conflict message must name the unordered node: %s", recorder.Body.String())
	}
	// Skipping intermediate nodes is legitimate when both endpoints have facts.
	if recorder := doRequest(t, router, http.MethodGet, chainPath("b-err", "dev", "staging"), ""); recorder.Code != http.StatusOK {
		t.Fatalf("non-adjacent nodes with facts status = %d (body %s)", recorder.Code, recorder.Body.String())
	}

	// Validation of the sequence happens before the batch NotFound check:
	// a malformed request stays a BadRequest even for an unknown batch.
	wantError(t, doRequest(t, router, http.MethodGet, chainPath("ghost", "dev", "dev"), ""),
		http.StatusBadRequest, store.CodePromotionSequenceInvalidV1)
}

func TestPromotionDiffBetweenAnyTwoNodes(t *testing.T) {
	router := newTestRouter(t)
	seedPromotionFixture(t, router, "b-diff2")

	recorder := doRequest(t, router, http.MethodGet,
		"/api/v1/release-batches/b-diff2/promotion-diff?from=dev&to=staging", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	body := decodeBody(t, recorder)
	if body["from_environment"] != "dev" || body["to_environment"] != "staging" {
		t.Fatalf("body = %v", body)
	}
	if body["consistent"] != false {
		t.Fatalf("consistent = %v", body["consistent"])
	}
	diff := body["diff"].(map[string]any)
	missing := diff["missing_changes"].([]any)
	if len(missing) != 1 || missing[0].(map[string]any)["title"] != "beta" {
		t.Fatalf("missing = %v", missing)
	}
	if body["from_release"].(map[string]any)["version"] != "1.0.0" {
		t.Fatalf("from_release = %v", body["from_release"])
	}

	// Nodes with no diff are consistent.
	recorder = doRequest(t, router, http.MethodGet,
		"/api/v1/release-batches/b-diff2/promotion-diff?from=staging&to=prod", "")
	body = decodeBody(t, recorder)
	if body["consistent"] != true || len(body["diff"].(map[string]any)["missing_changes"].([]any)) != 0 {
		t.Fatalf("staging->prod should be consistent: %v", body)
	}

	// Same node twice is BadRequest.
	wantError(t, doRequest(t, router, http.MethodGet,
		"/api/v1/release-batches/b-diff2/promotion-diff?from=dev&to=dev", ""),
		http.StatusBadRequest, store.CodeSamePromotionNodeV1)

	// Missing parameters.
	wantError(t, doRequest(t, router, http.MethodGet,
		"/api/v1/release-batches/b-diff2/promotion-diff?from=dev", ""),
		http.StatusBadRequest, store.CodePromotionSequenceInvalidV1)

	// Unknown environment / batch / unordered nodes reuse the canonical codes.
	wantError(t, doRequest(t, router, http.MethodGet,
		"/api/v1/release-batches/b-diff2/promotion-diff?from=dev&to=qa", ""),
		http.StatusBadRequest, store.CodePromotionSequenceInvalidV1)
	wantError(t, doRequest(t, router, http.MethodGet,
		"/api/v1/release-batches/ghost/promotion-diff?from=dev&to=test", ""),
		http.StatusNotFound, store.CodeReleaseBatchNotFoundV1)
}

func TestChangeTraceLocatesEntryFirstMissingAndPassedNodes(t *testing.T) {
	router := newTestRouter(t)
	seedPromotionFixture(t, router, "b-trace")

	getTrace := func(title string) map[string]any {
		recorder := doRequest(t, router, http.MethodGet,
			"/api/v1/release-batches/b-trace/changes/"+title+
				"/trace?environments=dev,test,staging,prod", "")
		if recorder.Code != http.StatusOK {
			t.Fatalf("trace %s status = %d (body %s)", title, recorder.Code, recorder.Body.String())
		}
		return decodeBody(t, recorder)
	}

	alpha := getTrace("alpha")
	if alpha["first_environment"] != "dev" || alpha["first_missing_environment"] != "" {
		t.Fatalf("alpha trace = %v", alpha)
	}
	passed := alpha["passed_environments"].([]any)
	if len(passed) != 4 || passed[0] != "dev" || passed[3] != "prod" {
		t.Fatalf("alpha passed = %v", passed)
	}
	if alpha["first_entered_at"] == "" {
		t.Fatalf("first_entered_at missing: %v", alpha)
	}

	beta := getTrace("beta")
	if beta["first_environment"] != "dev" {
		t.Fatalf("beta first env = %v", beta)
	}
	betaPassed := beta["passed_environments"].([]any)
	if len(betaPassed) != 2 || betaPassed[0] != "dev" || betaPassed[1] != "test" {
		t.Fatalf("beta passed = %v", betaPassed)
	}
	if beta["first_missing_environment"] != "staging" {
		t.Fatalf("beta first missing = %v, want staging", beta["first_missing_environment"])
	}

	// A change never present in the batch is its own NotFound.
	wantError(t, doRequest(t, router, http.MethodGet,
		"/api/v1/release-batches/b-trace/changes/nonexistent/trace?environments=dev,test", ""),
		http.StatusNotFound, store.CodePromotionChangeNotFoundV1)

	// Unknown batch / repeated env / unordered node keep canonical codes.
	wantError(t, doRequest(t, router, http.MethodGet,
		"/api/v1/release-batches/ghost/changes/alpha/trace?environments=dev,test", ""),
		http.StatusNotFound, store.CodeReleaseBatchNotFoundV1)
	wantError(t, doRequest(t, router, http.MethodGet,
		"/api/v1/release-batches/b-trace/changes/alpha/trace?environments=dev,dev", ""),
		http.StatusBadRequest, store.CodePromotionSequenceInvalidV1)
	registerEnvironmentOK(t, router, "qa")
	wantError(t, doRequest(t, router, http.MethodGet,
		"/api/v1/release-batches/b-trace/changes/alpha/trace?environments=dev,qa", ""),
		http.StatusConflict, store.CodePromotionOrderConflictV1)
}

func TestPromotionQueriesAreRepeatableAndStable(t *testing.T) {
	router := newTestRouter(t)
	seedPromotionFixture(t, router, "b-stable")
	target := chainPath("b-stable", "dev", "test", "staging", "prod")
	first := doRequest(t, router, http.MethodGet, target, "").Body.String()
	second := doRequest(t, router, http.MethodGet, target, "").Body.String()
	if first != second {
		t.Fatalf("repeated chain queries differ:\n%s\n%s", first, second)
	}
	diff1 := doRequest(t, router, http.MethodGet,
		"/api/v1/release-batches/b-stable/promotion-diff?from=dev&to=staging", "").Body.String()
	diff2 := doRequest(t, router, http.MethodGet,
		"/api/v1/release-batches/b-stable/promotion-diff?from=dev&to=staging", "").Body.String()
	if diff1 != diff2 {
		t.Fatalf("repeated diff queries differ:\n%s\n%s", diff1, diff2)
	}
}

func TestBatchedRecordWriteKeepsBackwardCompatibleShape(t *testing.T) {
	router := newTestRouter(t)
	registerEnvironmentOK(t, router, "prod")
	record := createRecordOK(t, router, batchRecordBody("b-compat", "prod", "1.0.0", "allowed", "rb:0.9.0", "a"))
	if record["batch_id"] != "b-compat" {
		t.Fatalf("batch_id = %v", record["batch_id"])
	}
	// Unbatched records keep the exact legacy shape without a batch_id key.
	legacy := createRecordOK(t, router, `{"environment":"prod","version":"2.0.0","changes":[
		{"category":"feature","title":"x","description":"d"}],
		"gate_status":"allowed","rollback_point":"rb:1.9.0"}`)
	if _, present := legacy["batch_id"]; present {
		t.Fatalf("unbatched record must not expose batch_id: %v", legacy)
	}
	// Blank batch_id is a validation failure, not a silent empty value.
	wantError(t, doRequest(t, router, http.MethodPost, "/api/v1/release-records",
		`{"environment":"prod","version":"3.0.0","batch_id":"   ","changes":[],
		"gate_status":"allowed","rollback_point":"rb:2.9.0"}`),
		http.StatusUnprocessableEntity, store.CodeReleaseValidationV1)
}
