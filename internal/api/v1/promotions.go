// Package v1 release-promotion queries associate the release facts of one
// batch across an ordered set of environments. All endpoints are read-only:
// release records are written solely through POST /api/v1/release-records.
package v1

import (
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/luwa07832/deployment-tracker/internal/store"
)

// releaseFactView is one release event in a batch at one environment node.
type releaseFactView struct {
	ID            string            `json:"id"`
	Environment   string            `json:"environment"`
	Version       string            `json:"version"`
	GateStatus    string            `json:"gate_status"`
	RollbackPoint string            `json:"rollback_point"`
	Changes       []changeEntryView `json:"changes"`
	RecordedAt    string            `json:"recorded_at"`
}

func toReleaseFactView(record *store.ReleaseRecord) releaseFactView {
	view := releaseFactView{
		ID:            record.PublicID,
		Environment:   record.Environment,
		Version:       record.Version,
		GateStatus:    record.GateStatus,
		RollbackPoint: record.RollbackPoint,
		Changes:       make([]changeEntryView, 0, len(record.Changes)),
		RecordedAt:    record.RecordedAt,
	}
	for _, entry := range record.Changes {
		view.Changes = append(view.Changes, changeEntryView(entry))
	}
	return view
}

// scalarConflictView reports whether a scalar fact differs between two
// adjacent promotion nodes.
type scalarConflictView struct {
	Left    string `json:"left"`
	Right   string `json:"right"`
	Changed bool   `json:"changed"`
}

// inconsistentChangeView pairs two change entries sharing a stable title
// identifier whose sequence, category or description differ.
type inconsistentChangeView struct {
	Left  changeEntryView `json:"left"`
	Right changeEntryView `json:"right"`
}

// segmentDiffView is the difference between two adjacent environment nodes
// in promotion order. Every category is always present with a deterministic
// empty array/object when it does not apply, so the shape is stable.
type segmentDiffView struct {
	FromEnvironment     string                   `json:"from_environment"`
	ToEnvironment       string                   `json:"to_environment"`
	AddedChanges        []changeEntryView        `json:"added_changes"`
	MissingChanges      []changeEntryView        `json:"missing_changes"`
	InconsistentChanges []inconsistentChangeView `json:"inconsistent_changes"`
	Version             scalarConflictView       `json:"version"`
	GateStatus          scalarConflictView       `json:"gate_status"`
	RollbackPoint       scalarConflictView       `json:"rollback_point"`
}

// promotionNodeView is one environment node with every release recorded for
// the batch there, in chronological order.
type promotionNodeView struct {
	Environment string            `json:"environment"`
	Releases    []releaseFactView `json:"releases"`
	Effective   releaseFactView   `json:"effective_release"`
}

type promotionChainView struct {
	BatchID      string              `json:"batch_id"`
	Environments []string            `json:"environments"`
	Nodes        []promotionNodeView `json:"nodes"`
	SegmentDiffs []segmentDiffView   `json:"segment_diffs"`
	Consistent   bool                `json:"consistent"`
}

type promotionDiffView struct {
	BatchID         string          `json:"batch_id"`
	FromEnvironment string          `json:"from_environment"`
	ToEnvironment   string          `json:"to_environment"`
	FromRelease     releaseFactView `json:"from_release"`
	ToRelease       releaseFactView `json:"to_release"`
	Diff            segmentDiffView `json:"diff"`
	Consistent      bool            `json:"consistent"`
}

type changeTraceView struct {
	BatchID                 string   `json:"batch_id"`
	ChangeTitle             string   `json:"change_title"`
	FirstEnvironment        string   `json:"first_environment"`
	FirstEnteredAt          string   `json:"first_entered_at"`
	Environments            []string `json:"environments"`
	PassedEnvironments      []string `json:"passed_environments"`
	FirstMissingEnvironment string   `json:"first_missing_environment"`
}

// parseEnvironmentSequence validates an ordered environment list: it must be
// non-empty, contain no duplicates and reference registered environments.
// On failure it writes the canonical error response and returns false.
func parseEnvironmentSequence(c *gin.Context, deps Dependencies, raw string) ([]string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		fail(c, http.StatusBadRequest, store.CodePromotionSequenceInvalidV1,
			"environments query parameter is required and must be an ordered, comma-separated list")
		return nil, false
	}
	parts := strings.Split(raw, ",")
	environments := make([]string, 0, len(parts))
	seen := map[string]bool{}
	duplicates := []string{}
	for _, part := range parts {
		key := strings.TrimSpace(part)
		if key == "" {
			fail(c, http.StatusBadRequest, store.CodePromotionSequenceInvalidV1,
				"environment sequence contains an empty environment")
			return nil, false
		}
		environments = append(environments, key)
		if seen[key] {
			duplicates = append(duplicates, key)
		}
		seen[key] = true
	}
	if len(duplicates) > 0 {
		sort.Strings(duplicates)
		fail(c, http.StatusBadRequest, store.CodePromotionSequenceInvalidV1,
			"environment sequence contains repeated environments: "+strings.Join(uniqueSorted(duplicates), ", "))
		return nil, false
	}
	unknown := []string{}
	for _, key := range environments {
		exists, err := deps.Store.TrackedEnvironmentExists(key)
		if err != nil {
			failStorage(c)
			return nil, false
		}
		if !exists {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		fail(c, http.StatusBadRequest, store.CodePromotionSequenceInvalidV1,
			"environment sequence contains unknown environments: "+strings.Join(uniqueSorted(unknown), ", "))
		return nil, false
	}
	return environments, true
}

// uniqueSorted deduplicates an already sorted slice of strings.
func uniqueSorted(values []string) []string {
	unique := make([]string, 0, len(values))
	var previous string
	for i, value := range values {
		if i > 0 && value == previous {
			continue
		}
		unique = append(unique, value)
		previous = value
	}
	return unique
}

// requireBatch writes RELEASE_BATCH_NOT_FOUND when no release record carries
// the batch identifier. It is the only observable NotFound for unknown
// batches and is checked after sequence validation.
func requireBatch(c *gin.Context, deps Dependencies, batchID string) bool {
	exists, err := deps.Store.ReleaseBatchExists(batchID)
	if err != nil {
		failStorage(c)
		return false
	}
	if !exists {
		fail(c, http.StatusNotFound, store.CodeReleaseBatchNotFoundV1,
			"no release batch with id "+batchID)
		return false
	}
	return true
}

// promotionRequest is the validated, batch-scoped environment selection.
type promotionRequest struct {
	BatchID      string
	Environments []string
}

func validatePromotionRequest(c *gin.Context, deps Dependencies) (promotionRequest, bool) {
	req := promotionRequest{
		BatchID:      strings.TrimSpace(c.Param("batch_id")),
		Environments: nil,
	}
	_, hasEnvironments := c.GetQuery("environments")
	rawRoute, hasRoute := c.GetQuery("route")
	if hasEnvironments && hasRoute {
		fail(c, http.StatusBadRequest, store.CodePromotionSelectorConflictV1,
			"environments and route selectors must not be provided together")
		return req, false
	}
	switch {
	case hasEnvironments:
		environments, ok := parseEnvironmentSequence(c, deps, c.Query("environments"))
		if !ok {
			return req, false
		}
		req.Environments = environments
	case hasRoute:
		name := strings.TrimSpace(rawRoute)
		if name == "" {
			fail(c, http.StatusNotFound, store.CodePromotionRouteNotFoundV1, "no such promotion route")
			return req, false
		}
		route, err := deps.Store.GetPromotionRoute(name)
		if err != nil {
			failStorage(c)
			return req, false
		}
		if route == nil {
			fail(c, http.StatusNotFound, store.CodePromotionRouteNotFoundV1, "no such promotion route "+name)
			return req, false
		}
		req.Environments = append(req.Environments, route.Environments...)
	default:
		// No explicit selector: the batch's bound route determines the
		// sequence. The binding (or lack of it) is observable before the
		// batch existence check.
		routeName, err := deps.Store.GetBoundRouteName(req.BatchID)
		if err != nil {
			failStorage(c)
			return req, false
		}
		if routeName == "" {
			fail(c, http.StatusBadRequest, store.CodePromotionSequenceInvalidV1,
				"environments or route selector is required when the batch is not bound to a promotion route")
			return req, false
		}
		route, err := deps.Store.GetPromotionRoute(routeName)
		if err != nil {
			failStorage(c)
			return req, false
		}
		if route == nil {
			fail(c, http.StatusNotFound, store.CodePromotionRouteNotFoundV1,
				"no such promotion route "+routeName)
			return req, false
		}
		req.Environments = append(req.Environments, route.Environments...)
	}
	if !requireBatch(c, deps, req.BatchID) {
		return req, false
	}
	return req, true
}

// loadBatchNodes fetches every release record of a batch and groups them by
// environment in chronological order (recorded_at ascending, ties broken by
// internal write order ascending). The returned map contains only nodes with
// at least one release fact.
func loadBatchNodes(deps Dependencies, batchID string) (map[string][]store.ReleaseRecord, error) {
	records, err := deps.Store.ListReleaseRecords(store.ReleaseRecordFilter{BatchID: batchID})
	if err != nil {
		return nil, err
	}
	// ListReleaseRecords returns records newest first (recorded_at desc, ties
	// broken by internal write order desc). Reversing yields chronological
	// order with write order ascending for same-timestamp facts, without
	// exposing the internal primary key.
	nodes := map[string][]store.ReleaseRecord{}
	for i := len(records) - 1; i >= 0; i-- {
		record := records[i]
		nodes[record.Environment] = append(nodes[record.Environment], record)
	}
	return nodes, nil
}

// missingNodes returns the requested environments that have no release fact
// in the batch, in request order.
func missingNodes(environments []string, nodes map[string][]store.ReleaseRecord) []string {
	missing := []string{}
	for _, key := range environments {
		if len(nodes[key]) == 0 {
			missing = append(missing, key)
		}
	}
	return missing
}

// getPromotionChain answers GET
// /api/v1/release-batches/:batch_id/promotion-chain.
func getPromotionChain(deps Dependencies) gin.HandlerFunc {
	return func(c *gin.Context) {
		req, ok := validatePromotionRequest(c, deps)
		if !ok {
			return
		}
		nodes, err := loadBatchNodes(deps, req.BatchID)
		if err != nil {
			failStorage(c)
			return
		}
		if missing := missingNodes(req.Environments, nodes); len(missing) > 0 {
			fail(c, http.StatusConflict, store.CodePromotionOrderConflictV1,
				"release facts are insufficient to order environment nodes: "+strings.Join(missing, ", "))
			return
		}
		nodeViews := make([]promotionNodeView, 0, len(req.Environments))
		effectiveByNode := map[string]*store.ReleaseRecord{}
		for _, key := range req.Environments {
			facts := nodes[key]
			view := promotionNodeView{
				Environment: key,
				Releases:    make([]releaseFactView, 0, len(facts)),
			}
			for i := range facts {
				view.Releases = append(view.Releases, toReleaseFactView(&facts[i]))
			}
			effective := &facts[len(facts)-1]
			view.Effective = toReleaseFactView(effective)
			effectiveByNode[key] = effective
			nodeViews = append(nodeViews, view)
		}
		segments := buildChainSegments(req.Environments, effectiveByNode)
		c.JSON(http.StatusOK, promotionChainView{
			BatchID:      req.BatchID,
			Environments: req.Environments,
			Nodes:        nodeViews,
			SegmentDiffs: segments,
			Consistent:   len(segments) == 0,
		})
	}
}

// buildChainSegments computes the diff for every adjacent pair of nodes.
// Pairs without any determinable difference are omitted; an empty slice means
// the whole chain is consistent.
func buildChainSegments(environments []string, effective map[string]*store.ReleaseRecord) []segmentDiffView {
	segments := []segmentDiffView{}
	for i := 0; i+1 < len(environments); i++ {
		segment := diffNodes(environments[i], environments[i+1],
			effective[environments[i]], effective[environments[i+1]])
		if !segmentIsEmpty(segment) {
			segments = append(segments, segment)
		}
	}
	return segments
}

func segmentIsEmpty(segment segmentDiffView) bool {
	return len(segment.AddedChanges) == 0 &&
		len(segment.MissingChanges) == 0 &&
		len(segment.InconsistentChanges) == 0 &&
		!segment.Version.Changed &&
		!segment.GateStatus.Changed &&
		!segment.RollbackPoint.Changed
}

// diffNodes compares two effective release facts in the from -> to direction.
// Change entries are identified by their stable title, matching the identity
// semantics of the existing environment comparison.
func diffNodes(fromName, toName string, from, to *store.ReleaseRecord) segmentDiffView {
	segment := segmentDiffView{
		FromEnvironment:     fromName,
		ToEnvironment:       toName,
		AddedChanges:        []changeEntryView{},
		MissingChanges:      []changeEntryView{},
		InconsistentChanges: []inconsistentChangeView{},
		Version: scalarConflictView{
			Left:    from.Version,
			Right:   to.Version,
			Changed: from.Version != to.Version,
		},
		GateStatus: scalarConflictView{
			Left:    from.GateStatus,
			Right:   to.GateStatus,
			Changed: from.GateStatus != to.GateStatus,
		},
		RollbackPoint: scalarConflictView{
			Left:    from.RollbackPoint,
			Right:   to.RollbackPoint,
			Changed: from.RollbackPoint != to.RollbackPoint,
		},
	}
	// Entries are grouped by title and paired inside each group by ascending
	// sequence, so every repeated-title entry of a historical record is
	// compared instead of only the last one surviving the title index.
	for _, pair := range pairChangesByTitle(from.Changes, to.Changes) {
		switch {
		case pair.Left == nil:
			segment.AddedChanges = append(segment.AddedChanges, toChangeEntryView(*pair.Right))
		case pair.Right == nil:
			segment.MissingChanges = append(segment.MissingChanges, toChangeEntryView(*pair.Left))
		case !sameEntry(*pair.Left, *pair.Right):
			segment.InconsistentChanges = append(segment.InconsistentChanges, inconsistentChangeView{
				Left:  toChangeEntryView(*pair.Left),
				Right: toChangeEntryView(*pair.Right),
			})
		}
	}
	sortEntriesByTitle(segment.AddedChanges)
	sortEntriesByTitle(segment.MissingChanges)
	sort.Slice(segment.InconsistentChanges, func(i, j int) bool {
		return entryOrder(segment.InconsistentChanges[i].Left) < entryOrder(segment.InconsistentChanges[j].Left)
	})
	return segment
}

// sortEntriesByTitle orders change views by title then sequence.
func sortEntriesByTitle(entries []changeEntryView) {
	sort.Slice(entries, func(i, j int) bool {
		return entryOrder(entries[i]) < entryOrder(entries[j])
	})
}

// getPromotionDiff answers GET
// /api/v1/release-batches/:batch_id/promotion-diff with any two nodes.
func getPromotionDiff(deps Dependencies) gin.HandlerFunc {
	return func(c *gin.Context) {
		from := strings.TrimSpace(c.Query("from"))
		to := strings.TrimSpace(c.Query("to"))
		if from == "" || to == "" {
			fail(c, http.StatusBadRequest, store.CodePromotionSequenceInvalidV1,
				"from and to query parameters are required")
			return
		}
		if from == to {
			fail(c, http.StatusBadRequest, store.CodeSamePromotionNodeV1,
				"cannot diff an environment node with itself: "+from)
			return
		}
		sequence, ok := parseEnvironmentSequence(c, deps, from+","+to)
		if !ok {
			return
		}
		batchID := strings.TrimSpace(c.Param("batch_id"))
		if !requireBatch(c, deps, batchID) {
			return
		}
		nodes, err := loadBatchNodes(deps, batchID)
		if err != nil {
			failStorage(c)
			return
		}
		if missing := missingNodes(sequence, nodes); len(missing) > 0 {
			fail(c, http.StatusConflict, store.CodePromotionOrderConflictV1,
				"release facts are insufficient to order environment nodes: "+strings.Join(missing, ", "))
			return
		}
		fromFacts := nodes[sequence[0]]
		toFacts := nodes[sequence[1]]
		fromEffective := &fromFacts[len(fromFacts)-1]
		toEffective := &toFacts[len(toFacts)-1]
		diff := diffNodes(sequence[0], sequence[1], fromEffective, toEffective)
		c.JSON(http.StatusOK, promotionDiffView{
			BatchID:         batchID,
			FromEnvironment: sequence[0],
			ToEnvironment:   sequence[1],
			FromRelease:     toReleaseFactView(fromEffective),
			ToRelease:       toReleaseFactView(toEffective),
			Diff:            diff,
			Consistent:      segmentIsEmpty(diff),
		})
	}
}

// tracePromotionChange answers GET
// /api/v1/release-batches/:batch_id/changes/:title/trace.
func tracePromotionChange(deps Dependencies) gin.HandlerFunc {
	return func(c *gin.Context) {
		req, ok := validatePromotionRequest(c, deps)
		if !ok {
			return
		}
		title := c.Param("title")
		nodes, err := loadBatchNodes(deps, req.BatchID)
		if err != nil {
			failStorage(c)
			return
		}
		if missing := missingNodes(req.Environments, nodes); len(missing) > 0 {
			fail(c, http.StatusConflict, store.CodePromotionOrderConflictV1,
				"release facts are insufficient to order environment nodes: "+strings.Join(missing, ", "))
			return
		}
		effective := map[string]*store.ReleaseRecord{}
		for _, key := range req.Environments {
			facts := nodes[key]
			effective[key] = &facts[len(facts)-1]
		}
		firstIndex := -1
		var firstEntry store.ChangeEntry
		for i, key := range req.Environments {
			if entry, ok := firstChangeByTitle(effective[key].Changes, title); ok {
				firstIndex = i
				firstEntry = entry
				break
			}
		}
		if firstIndex < 0 {
			fail(c, http.StatusNotFound, store.CodePromotionChangeNotFoundV1,
				"change "+title+" is not part of release batch "+req.BatchID)
			return
		}
		passed := []string{}
		for _, key := range req.Environments[firstIndex:] {
			if containsChangeTitle(effective[key].Changes, title) {
				passed = append(passed, key)
			} else {
				break
			}
		}
		firstMissing := ""
		for _, key := range req.Environments[firstIndex+1:] {
			if !containsChangeTitle(effective[key].Changes, title) {
				firstMissing = key
				break
			}
		}
		firstEnteredAt := firstReleaseTimeForChange(nodes[req.Environments[firstIndex]], title)
		c.JSON(http.StatusOK, changeTraceView{
			BatchID:                 req.BatchID,
			ChangeTitle:             firstEntry.Title,
			FirstEnvironment:        req.Environments[firstIndex],
			FirstEnteredAt:          firstEnteredAt,
			Environments:            req.Environments,
			PassedEnvironments:      passed,
			FirstMissingEnvironment: firstMissing,
		})
	}
}

// firstReleaseTimeForChange returns the recorded_at of the earliest release
// fact at one node that carries the titled change entry.
func firstReleaseTimeForChange(facts []store.ReleaseRecord, title string) string {
	for i := range facts {
		if containsChangeTitle(facts[i].Changes, title) {
			return facts[i].RecordedAt
		}
	}
	return ""
}
