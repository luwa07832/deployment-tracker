package v1

import (
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/luwa07832/deployment-tracker/internal/store"
)

// Promotion-chain inconsistency categories. They are stable public strings and
// each one names exactly one divergence kind, never a merged catch-all.
const (
	categoryVersionDivergence = "version_divergence"
	categoryGateConflict      = "gate_status_conflict"
	categoryRollbackConflict  = "rollback_point_conflict"
	categoryMissingChanges    = "missing_changes"
	categoryChangedChanges    = "inconsistent_changes"
)

type chainReleaseView struct {
	ID            string            `json:"id"`
	Environment   string            `json:"environment"`
	Version       string            `json:"version"`
	GateStatus    string            `json:"gate_status"`
	RollbackPoint string            `json:"rollback_point"`
	Changes       []changeEntryView `json:"changes"`
	RecordedAt    string            `json:"recorded_at"`
}

type chainNodeView struct {
	Environment string             `json:"environment"`
	Releases    []chainReleaseView `json:"releases"`
}

type versionDivergenceView struct {
	Version    string `json:"version"`
	Presence   string `json:"presence"`
	OnlyInFrom bool   `json:"only_in_from"`
	OnlyInTo   bool   `json:"only_in_to"`
}

type segmentDiffView struct {
	From                string                  `json:"from_environment"`
	To                  string                  `json:"to_environment"`
	Consistent          bool                    `json:"consistent"`
	Categories          []string                `json:"categories"`
	VersionDivergences  []versionDivergenceView `json:"version_divergences"`
	GateConflicts       []versionGateView       `json:"gate_status_conflicts"`
	RollbackConflicts   []versionFieldDiffView  `json:"rollback_point_conflicts"`
	AddedChanges        []changeEntryView       `json:"added_changes"`
	MissingChanges      []changeEntryView       `json:"missing_changes"`
	InconsistentChanges []changedEntryView      `json:"inconsistent_changes"`
}

type versionGateView struct {
	Version string `json:"version"`
	From    string `json:"from_gate_status"`
	To      string `json:"to_gate_status"`
}

type versionFieldDiffView struct {
	Version string `json:"version"`
	Field   string `json:"field"`
	From    string `json:"from"`
	To      string `json:"to"`
}

type chainView struct {
	BatchID      string            `json:"batch_id"`
	Environments []string          `json:"environment_order"`
	Nodes        []chainNodeView   `json:"nodes"`
	SegmentDiffs []segmentDiffView `json:"segment_diffs"`
	Consistent   bool              `json:"consistent"`
	Categories   []string          `json:"inconsistency_categories"`
}

type localDiffView struct {
	BatchID         string          `json:"batch_id"`
	FromEnvironment string          `json:"from_environment"`
	ToEnvironment   string          `json:"to_environment"`
	Segment         segmentDiffView `json:"segment"`
}

type changeAppearanceView struct {
	Environment   string `json:"environment"`
	ReleaseID     string `json:"release_id"`
	Version       string `json:"version"`
	RecordedAt    string `json:"recorded_at"`
	GateStatus    string `json:"gate_status"`
	RollbackPoint string `json:"rollback_point"`
}

type changeTraceView struct {
	BatchID             string                 `json:"batch_id"`
	ChangeID            string                 `json:"change_id"`
	Environments        []string               `json:"environment_order"`
	FirstPresent        *string                `json:"first_present_environment"`
	FirstMissing        *string                `json:"first_missing_environment"`
	PresentEnvironments []string               `json:"present_environments"`
	Appearances         []changeAppearanceView `json:"appearances"`
}

func toChainReleaseView(record *store.ReleaseRecord) chainReleaseView {
	changes := make([]changeEntryView, 0, len(record.Changes))
	for _, entry := range record.Changes {
		changes = append(changes, changeEntryView(entry))
	}
	return chainReleaseView{
		ID:            record.PublicID,
		Environment:   record.Environment,
		Version:       record.Version,
		GateStatus:    record.GateStatus,
		RollbackPoint: record.RollbackPoint,
		Changes:       changes,
		RecordedAt:    record.RecordedAt,
	}
}

// resolveChainSequence validates the ordered environment sequence against the
// request rules. It returns the trimmed unique sequence or writes the single
// applicable error response.
func resolveChainSequence(c *gin.Context, deps Dependencies, raw string) ([]string, bool) {
	parts := strings.Split(raw, ",")
	environments := make([]string, 0, len(parts))
	for _, part := range parts {
		environments = append(environments, strings.TrimSpace(part))
	}
	if len(environments) == 0 || (len(environments) == 1 && environments[0] == "") {
		fail(c, http.StatusBadRequest, store.CodeInvalidRequest, "environments query parameter is required")
		return nil, false
	}
	seen := map[string]bool{}
	duplicates := []string{}
	seenDup := map[string]bool{}
	for _, env := range environments {
		if env == "" {
			fail(c, http.StatusBadRequest, store.CodeInvalidRequest, "environments entries must not be empty")
			return nil, false
		}
		if seen[env] && !seenDup[env] {
			duplicates = append(duplicates, env)
			seenDup[env] = true
		}
		seen[env] = true
	}
	if len(duplicates) > 0 {
		sort.Strings(duplicates)
		fail(c, http.StatusBadRequest, store.CodeDuplicateEnvironmentChainV1,
			"environments sequence repeats environment(s): "+strings.Join(duplicates, ", "))
		return nil, false
	}
	unknown := []string{}
	for _, env := range environments {
		exists, err := deps.Store.TrackedEnvironmentExists(env)
		if err != nil {
			failStorage(c)
			return nil, false
		}
		if !exists {
			unknown = append(unknown, env)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		fail(c, http.StatusBadRequest, store.CodeUnknownEnvironmentChainV1,
			"environments sequence contains unknown environment(s): "+strings.Join(unknown, ", "))
		return nil, false
	}
	return environments, true
}

// nodeFacts holds the chronological release facts of one environment node
// within a batch. Records are already stored oldest first.
type nodeFacts struct {
	environment string
	records     []store.ReleaseRecord
}

func loadBatchNodes(deps Dependencies, batchID string, environments []string) (map[string]*nodeFacts, error) {
	records, err := deps.Store.ListBatchReleaseRecords(batchID)
	if err != nil {
		return nil, err
	}
	nodes := make(map[string]*nodeFacts, len(environments))
	for _, env := range environments {
		nodes[env] = &nodeFacts{environment: env, records: nil}
	}
	for i := range records {
		node, ok := nodes[records[i].Environment]
		if !ok {
			continue
		}
		node.records = append(node.records, records[i])
	}
	return nodes, nil
}

// orderConflicts returns the adjacent environment pairs whose promotion order
// cannot be determined from the stored release facts. A pair is unordered when
// a node has no facts, when shared versions prove a later->earlier direction,
// or (without a shared version) when the fact windows overlap instead of
// advancing.
func orderConflicts(environments []string, nodes map[string]*nodeFacts) []string {
	conflicts := []string{}
	for i := 0; i+1 < len(environments); i++ {
		from := nodes[environments[i]]
		to := nodes[environments[i+1]]
		if len(from.records) == 0 || len(to.records) == 0 {
			conflicts = append(conflicts, environments[i]+" -> "+environments[i+1])
			continue
		}
		if !factsAdvance(from, to) {
			conflicts = append(conflicts, environments[i]+" -> "+environments[i+1])
		}
	}
	return conflicts
}

// factsAdvance reports whether the facts on "to" provably happened after the
// facts on "from" along the promotion direction.
func factsAdvance(from, to *nodeFacts) bool {
	fromByVersion := indexByVersion(from.records)
	toByVersion := indexByVersion(to.records)
	shared := make([]string, 0)
	for version := range fromByVersion {
		if _, ok := toByVersion[version]; ok {
			shared = append(shared, version)
		}
	}
	if len(shared) > 0 {
		for _, version := range shared {
			fromLast := fromByVersion[version][len(fromByVersion[version])-1]
			toLast := toByVersion[version][len(toByVersion[version])-1]
			if toLast.RecordedAt <= fromLast.RecordedAt {
				return false
			}
		}
		return true
	}
	// No shared version: the whole target window must start strictly after the
	// whole source window, otherwise the facts cannot establish an order.
	return to.records[0].RecordedAt > from.records[len(from.records)-1].RecordedAt
}

func indexByVersion(records []store.ReleaseRecord) map[string][]store.ReleaseRecord {
	indexed := map[string][]store.ReleaseRecord{}
	for i := range records {
		indexed[records[i].Version] = append(indexed[records[i].Version], records[i])
	}
	return indexed
}

// buildChain assembles the stable chain response. Segment differences compare
// the full fact timeline of adjacent nodes version by version.
func buildChain(batchID string, environments []string, nodes map[string]*nodeFacts) chainView {
	nodeViews := make([]chainNodeView, 0, len(environments))
	for _, env := range environments {
		node := nodes[env]
		releases := make([]chainReleaseView, 0, len(node.records))
		for i := range node.records {
			releases = append(releases, toChainReleaseView(&node.records[i]))
		}
		nodeViews = append(nodeViews, chainNodeView{Environment: env, Releases: releases})
	}
	segments := make([]segmentDiffView, 0, len(environments)-1)
	categorySet := map[string]bool{}
	allConsistent := true
	for i := 0; i+1 < len(environments); i++ {
		segment := diffSegment(environments[i], environments[i+1],
			nodes[environments[i]], nodes[environments[i+1]])
		segments = append(segments, segment)
		if !segment.Consistent {
			allConsistent = false
			for _, category := range segment.Categories {
				categorySet[category] = true
			}
		}
	}
	categories := make([]string, 0, len(categorySet))
	for category := range categorySet {
		categories = append(categories, category)
	}
	sort.Strings(categories)
	order := make([]string, len(environments))
	copy(order, environments)
	return chainView{
		BatchID:      batchID,
		Environments: order,
		Nodes:        nodeViews,
		SegmentDiffs: segments,
		Consistent:   allConsistent,
		Categories:   categories,
	}
}

// diffSegment computes the promotion difference from one environment node to
// the next. Every version on exactly one side is a version divergence; shared
// versions are compared on gate status, rollback point and change entries.
// Added changes alone do not make a segment inconsistent: promotion carries
// them forward. Missing or inconsistent changes, version divergence, gate and
// rollback conflicts do.
func diffSegment(fromEnv, toEnv string, from, to *nodeFacts) segmentDiffView {
	segment := segmentDiffView{
		From:                fromEnv,
		To:                  toEnv,
		Consistent:          true,
		Categories:          []string{},
		VersionDivergences:  []versionDivergenceView{},
		GateConflicts:       []versionGateView{},
		RollbackConflicts:   []versionFieldDiffView{},
		AddedChanges:        []changeEntryView{},
		MissingChanges:      []changeEntryView{},
		InconsistentChanges: []changedEntryView{},
	}
	fromByVersion := indexByVersion(from.records)
	toByVersion := indexByVersion(to.records)
	versions := make(map[string]bool, len(fromByVersion)+len(toByVersion))
	for version := range fromByVersion {
		versions[version] = true
	}
	for version := range toByVersion {
		versions[version] = true
	}
	orderedVersions := make([]string, 0, len(versions))
	for version := range versions {
		orderedVersions = append(orderedVersions, version)
	}
	sortVersions(orderedVersions)
	addedSet := map[string]changeEntryView{}
	missingSet := map[string]changeEntryView{}
	changedSet := map[string]changedEntryView{}
	for _, version := range orderedVersions {
		fromRecords := fromByVersion[version]
		toRecords := toByVersion[version]
		switch {
		case len(fromRecords) > 0 && len(toRecords) == 0:
			segment.VersionDivergences = append(segment.VersionDivergences, versionDivergenceView{
				Version: version, Presence: "from", OnlyInFrom: true,
			})
		case len(fromRecords) == 0 && len(toRecords) > 0:
			segment.VersionDivergences = append(segment.VersionDivergences, versionDivergenceView{
				Version: version, Presence: "to", OnlyInTo: true,
			})
		default:
			fromRecord := fromRecords[len(fromRecords)-1]
			toRecord := toRecords[len(toRecords)-1]
			if fromRecord.GateStatus != toRecord.GateStatus {
				segment.GateConflicts = append(segment.GateConflicts, versionGateView{
					Version: version, From: fromRecord.GateStatus, To: toRecord.GateStatus,
				})
			}
			if fromRecord.RollbackPoint != toRecord.RollbackPoint {
				segment.RollbackConflicts = append(segment.RollbackConflicts, versionFieldDiffView{
					Version: version, Field: "rollback_point",
					From: fromRecord.RollbackPoint, To: toRecord.RollbackPoint,
				})
			}
			collectEntryDiffs(fromRecord.Changes, toRecord.Changes, addedSet, missingSet, changedSet)
		}
	}
	for _, entry := range addedSet {
		segment.AddedChanges = append(segment.AddedChanges, entry)
	}
	for _, entry := range missingSet {
		segment.MissingChanges = append(segment.MissingChanges, entry)
	}
	for _, entry := range changedSet {
		segment.InconsistentChanges = append(segment.InconsistentChanges, entry)
	}
	sortEntries(segment.AddedChanges)
	sortEntries(segment.MissingChanges)
	sort.Slice(segment.InconsistentChanges, func(i, j int) bool {
		return entryOrder(segment.InconsistentChanges[i].Left) < entryOrder(segment.InconsistentChanges[j].Left)
	})
	addCategory := func(category string) {
		segment.Consistent = false
		for _, existing := range segment.Categories {
			if existing == category {
				return
			}
		}
		segment.Categories = append(segment.Categories, category)
	}
	if len(segment.VersionDivergences) > 0 {
		addCategory(categoryVersionDivergence)
	}
	if len(segment.GateConflicts) > 0 {
		addCategory(categoryGateConflict)
	}
	if len(segment.RollbackConflicts) > 0 {
		addCategory(categoryRollbackConflict)
	}
	if len(segment.MissingChanges) > 0 {
		addCategory(categoryMissingChanges)
	}
	if len(segment.InconsistentChanges) > 0 {
		addCategory(categoryChangedChanges)
	}
	sort.Strings(segment.Categories)
	return segment
}

// collectEntryDiffs merges one shared version's entry differences into the
// segment-level sets keyed by the stable change identifier (title). When a
// change is both added at one version and missing at another, the missing
// observation wins: the segment overall drops the change.
func collectEntryDiffs(fromEntries, toEntries []store.ChangeEntry,
	added, missing map[string]changeEntryView, changed map[string]changedEntryView) {
	fromIndex := indexEntries(fromEntries)
	toIndex := indexEntries(toEntries)
	for title, toEntry := range toIndex {
		fromEntry, present := fromIndex[title]
		view := changeEntryView(toEntry)
		if !present {
			delete(missing, title)
			if _, already := added[title]; !already {
				added[title] = view
			}
			continue
		}
		if !sameEntry(fromEntry, toEntry) {
			changed[title] = changedEntryView{
				Left:  changeEntryView(fromEntry),
				Right: view,
			}
		}
		delete(added, title)
	}
	for title, fromEntry := range fromIndex {
		if _, present := toIndex[title]; !present {
			delete(added, title)
			missing[title] = changeEntryView(fromEntry)
		}
	}
}

// chainQuery holds the common preprocessing for the read-only promotion
// endpoints: sequence validation, batch existence and order determinability.
type chainQuery struct {
	batchID      string
	environments []string
	nodes        map[string]*nodeFacts
}

func prepareChainQuery(c *gin.Context, deps Dependencies) (chainQuery, bool) {
	batchID := strings.TrimSpace(c.Param("batch_id"))
	if batchID == "" {
		fail(c, http.StatusBadRequest, store.CodeInvalidRequest, "batch_id is required")
		return chainQuery{}, false
	}
	environments, ok := resolveChainSequence(c, deps, c.Query("environments"))
	if !ok {
		return chainQuery{}, false
	}
	exists, err := deps.Store.BatchExists(batchID)
	if err != nil {
		failStorage(c)
		return chainQuery{}, false
	}
	if !exists {
		fail(c, http.StatusNotFound, store.CodeReleaseBatchNotFoundV1,
			"no release batch with id "+batchID)
		return chainQuery{}, false
	}
	nodes, err := loadBatchNodes(deps, batchID, environments)
	if err != nil {
		failStorage(c)
		return chainQuery{}, false
	}
	if conflicts := orderConflicts(environments, nodes); len(conflicts) > 0 {
		fail(c, http.StatusConflict, store.CodePromotionChainConflictV1,
			"release facts cannot determine promotion order for node(s): "+strings.Join(conflicts, "; "))
		return chainQuery{}, false
	}
	return chainQuery{batchID: batchID, environments: environments, nodes: nodes}, true
}

// getPromotionChain answers GET /release-batches/:batch_id/chain.
func getPromotionChain(deps Dependencies) gin.HandlerFunc {
	return func(c *gin.Context) {
		query, ok := prepareChainQuery(c, deps)
		if !ok {
			return
		}
		c.JSON(http.StatusOK, buildChain(query.batchID, query.environments, query.nodes))
	}
}

// getLocalPromotionDiff answers the partial difference between any two nodes:
// GET /release-batches/:batch_id/promotion-diff?from=&to=.
func getLocalPromotionDiff(deps Dependencies) gin.HandlerFunc {
	return func(c *gin.Context) {
		batchID := strings.TrimSpace(c.Param("batch_id"))
		if batchID == "" {
			fail(c, http.StatusBadRequest, store.CodeInvalidRequest, "batch_id is required")
			return
		}
		from := strings.TrimSpace(c.Query("from"))
		to := strings.TrimSpace(c.Query("to"))
		if from == "" || to == "" {
			fail(c, http.StatusBadRequest, store.CodeInvalidRequest,
				"from and to query parameters are required")
			return
		}
		if from == to {
			fail(c, http.StatusBadRequest, store.CodeSameEnvironmentCompareV1,
				"cannot diff an environment with itself")
			return
		}
		query, ok := prepareLocalDiff(c, deps, batchID, from, to)
		if !ok {
			return
		}
		segment := diffSegment(from, to, query.nodes[from], query.nodes[to])
		c.JSON(http.StatusOK, localDiffView{
			BatchID:         batchID,
			FromEnvironment: from,
			ToEnvironment:   to,
			Segment:         segment,
		})
	}
}

func prepareLocalDiff(c *gin.Context, deps Dependencies, batchID, from, to string) (chainQuery, bool) {
	environments, ok := resolveChainSequence(c, deps, from+","+to)
	if !ok {
		return chainQuery{}, false
	}
	exists, err := deps.Store.BatchExists(batchID)
	if err != nil {
		failStorage(c)
		return chainQuery{}, false
	}
	if !exists {
		fail(c, http.StatusNotFound, store.CodeReleaseBatchNotFoundV1,
			"no release batch with id "+batchID)
		return chainQuery{}, false
	}
	nodes, err := loadBatchNodes(deps, batchID, environments)
	if err != nil {
		failStorage(c)
		return chainQuery{}, false
	}
	if conflicts := orderConflicts(environments, nodes); len(conflicts) > 0 {
		fail(c, http.StatusConflict, store.CodePromotionChainConflictV1,
			"release facts cannot determine promotion order for node(s): "+strings.Join(conflicts, "; "))
		return chainQuery{}, false
	}
	return chainQuery{batchID: batchID, environments: environments, nodes: nodes}, true
}

// getChangeTrace answers the trace of one change entry through the chain:
// GET /release-batches/:batch_id/changes/:change_id/trace?environments=...
func getChangeTrace(deps Dependencies) gin.HandlerFunc {
	return func(c *gin.Context) {
		changeID := strings.TrimSpace(c.Param("change_id"))
		if changeID == "" {
			fail(c, http.StatusBadRequest, store.CodeInvalidRequest, "change_id is required")
			return
		}
		query, ok := prepareChainQuery(c, deps)
		if !ok {
			return
		}
		c.JSON(http.StatusOK, buildChangeTrace(query.batchID, changeID, query.environments, query.nodes))
	}
}

// buildChangeTrace locates the first node carrying the change, every node it
// passes through, and the first following node where it is absent. A change
// absent from the whole chain yields null first-node fields and empty arrays,
// not an error.
func buildChangeTrace(batchID, changeID string, environments []string, nodes map[string]*nodeFacts) changeTraceView {
	presentEnvironments := []string{}
	appearances := []changeAppearanceView{}
	firstPresentIndex := -1
	for index, env := range environments {
		node := nodes[env]
		nodePresent := false
		for i := range node.records {
			record := &node.records[i]
			for _, entry := range record.Changes {
				if entry.Title == changeID {
					nodePresent = true
					appearances = append(appearances, changeAppearanceView{
						Environment:   env,
						ReleaseID:     record.PublicID,
						Version:       record.Version,
						RecordedAt:    record.RecordedAt,
						GateStatus:    record.GateStatus,
						RollbackPoint: record.RollbackPoint,
					})
					break
				}
			}
		}
		if nodePresent {
			presentEnvironments = append(presentEnvironments, env)
			if firstPresentIndex == -1 {
				firstPresentIndex = index
			}
		}
	}
	order := make([]string, len(environments))
	copy(order, environments)
	trace := changeTraceView{
		BatchID:             batchID,
		ChangeID:            changeID,
		Environments:        order,
		FirstPresent:        nil,
		FirstMissing:        nil,
		PresentEnvironments: presentEnvironments,
		Appearances:         appearances,
	}
	if firstPresentIndex != -1 {
		firstPresent := environments[firstPresentIndex]
		trace.FirstPresent = &firstPresent
		for index := firstPresentIndex + 1; index < len(environments); index++ {
			if !containsString(presentEnvironments, environments[index]) {
				firstMissing := environments[index]
				trace.FirstMissing = &firstMissing
				break
			}
		}
	}
	return trace
}

func containsString(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}
