package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/luwa07832/deployment-tracker/internal/store"
)

// releaseResponse is the JSON view of one release record. Fields introduced
// after the initial schema are omitted for records that predate them.
type releaseResponse struct {
	ID            int64     `json:"id"`
	Environment   string    `json:"environment"`
	Version       string    `json:"version"`
	Changes       *[]string `json:"changes,omitempty"`
	GateStatus    *string   `json:"gate_status,omitempty"`
	RollbackPoint *string   `json:"rollback_point,omitempty"`
	RegisteredAt  string    `json:"registered_at"`
}

func toReleaseResponse(rel *store.Release) releaseResponse {
	resp := releaseResponse{
		ID:            rel.ID,
		Environment:   rel.Environment,
		Version:       rel.Version,
		GateStatus:    rel.GateStatus,
		RollbackPoint: rel.RollbackPoint,
		RegisteredAt:  rel.CreatedAt,
	}
	if rel.Changes != nil {
		changes := rel.Changes
		resp.Changes = &changes
	}
	return resp
}

func toReleaseList(releases []store.Release) []releaseResponse {
	out := make([]releaseResponse, 0, len(releases))
	for i := range releases {
		out = append(out, toReleaseResponse(&releases[i]))
	}
	return out
}

// createReleaseInput is the accepted request body for POST /releases. Pointer
// fields let validation tell "missing" apart from "empty".
type createReleaseInput struct {
	Name          *string   `json:"name"`
	Environment   *string   `json:"environment"`
	Version       *string   `json:"version"`
	Changes       *[]string `json:"changes"`
	GateStatus    *string   `json:"gate_status"`
	RollbackPoint *string   `json:"rollback_point"`
}

var allowedGateStatuses = map[string]bool{"allowed": true, "blocked": true, "pending": true}

// createRelease registers one immutable release record. Re-registering the
// same environment and version with identical content returns the existing
// record; different content is a conflict.
func createRelease(deps Dependencies) gin.HandlerFunc {
	return func(c *gin.Context) {
		var input createReleaseInput
		if err := json.NewDecoder(c.Request.Body).Decode(&input); err != nil {
			var typeErr *json.UnmarshalTypeError
			if errors.As(err, &typeErr) {
				fail(c, http.StatusBadRequest, store.CodeInvalidReleaseInput, "field "+typeErr.Field+" has an invalid type")
				return
			}
			fail(c, http.StatusBadRequest, store.CodeInvalidRequest, "request body must be valid JSON")
			return
		}
		rel, problem := validateReleaseInput(&input)
		if problem != "" {
			fail(c, http.StatusBadRequest, store.CodeInvalidReleaseInput, problem)
			return
		}
		existing, err := deps.Store.FindRelease(rel.Environment, rel.Version)
		if err != nil {
			fail(c, http.StatusInternalServerError, store.CodeStorageUnavailable, "database is not available")
			return
		}
		if existing != nil {
			if sameReleaseContent(existing, rel) {
				c.JSON(http.StatusOK, gin.H{"release": toReleaseResponse(existing)})
				return
			}
			fail(c, http.StatusConflict, store.CodeReleaseConflict, "release "+rel.Environment+" "+rel.Version+" already exists with different content")
			return
		}
		if err := deps.Store.InsertRelease(rel); err != nil {
			fail(c, http.StatusInternalServerError, store.CodeStorageUnavailable, "database is not available")
			return
		}
		c.JSON(http.StatusCreated, gin.H{"release": toReleaseResponse(rel)})
	}
}

// validateReleaseInput applies the field rules for newly created records. It
// returns a ready-to-store record or a one-sentence problem description.
func validateReleaseInput(input *createReleaseInput) (*store.Release, string) {
	if input.Environment == nil || strings.TrimSpace(*input.Environment) == "" {
		return nil, "environment is required"
	}
	if input.Version == nil || strings.TrimSpace(*input.Version) == "" {
		return nil, "version is required"
	}
	if input.Changes == nil {
		return nil, "changes is required"
	}
	if input.GateStatus == nil || *input.GateStatus == "" {
		return nil, "gate_status is required"
	}
	if !allowedGateStatuses[*input.GateStatus] {
		return nil, "gate_status must be one of: allowed, blocked, pending"
	}
	if input.RollbackPoint == nil || strings.TrimSpace(*input.RollbackPoint) == "" {
		return nil, "rollback_point is required"
	}
	changes := make([]string, 0, len(*input.Changes))
	for _, entry := range *input.Changes {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			return nil, "changes entries must be non-empty strings"
		}
		changes = append(changes, entry)
	}
	name := ""
	if input.Name != nil {
		name = strings.TrimSpace(*input.Name)
	}
	gateStatus := *input.GateStatus
	rollbackPoint := strings.TrimSpace(*input.RollbackPoint)
	return &store.Release{
		Name:          name,
		Environment:   strings.TrimSpace(*input.Environment),
		Version:       strings.TrimSpace(*input.Version),
		Changes:       changes,
		GateStatus:    &gateStatus,
		RollbackPoint: &rollbackPoint,
	}, ""
}

// sameReleaseContent reports whether an existing record and a new submission
// carry identical content. Change entries compare as a set.
func sameReleaseContent(existing, next *store.Release) bool {
	if !equalStringPtr(existing.GateStatus, next.GateStatus) {
		return false
	}
	if !equalStringPtr(existing.RollbackPoint, next.RollbackPoint) {
		return false
	}
	return stringSetEqual(existing.Changes, next.Changes)
}

func equalStringPtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func stringSetEqual(a, b []string) bool {
	counts := make(map[string]int, len(a))
	for _, entry := range a {
		counts[entry]++
	}
	for _, entry := range b {
		counts[entry]--
	}
	for _, count := range counts {
		if count != 0 {
			return false
		}
	}
	return true
}

// listReleases answers filtered queries. Every filter is optional and filters
// combine with AND. An unknown environment is an error; any other empty
// result is an empty list.
func listReleases(deps Dependencies) gin.HandlerFunc {
	return func(c *gin.Context) {
		filter := store.ReleaseFilter{
			Environment:   c.Query("environment"),
			Version:       c.Query("version"),
			Change:        c.Query("change"),
			GateStatus:    c.Query("gate_status"),
			RollbackPoint: c.Query("rollback_point"),
		}
		if filter.Environment != "" && !environmentExists(c, deps, filter.Environment) {
			return
		}
		releases, err := deps.Store.ListReleases(filter)
		if err != nil {
			fail(c, http.StatusInternalServerError, store.CodeStorageUnavailable, "database is not available")
			return
		}
		c.JSON(http.StatusOK, gin.H{"releases": toReleaseList(releases)})
	}
}

// getRelease returns the effective record for one environment and version.
func getRelease(deps Dependencies) gin.HandlerFunc {
	return func(c *gin.Context) {
		environment := c.Param("environment")
		version := c.Param("version")
		if !environmentExists(c, deps, environment) {
			return
		}
		rel, err := deps.Store.FindRelease(environment, version)
		if err != nil {
			fail(c, http.StatusInternalServerError, store.CodeStorageUnavailable, "database is not available")
			return
		}
		if rel == nil {
			fail(c, http.StatusNotFound, store.CodeReleaseNotFound, "no release recorded for "+environment+" "+version)
			return
		}
		c.JSON(http.StatusOK, gin.H{"release": toReleaseResponse(rel)})
	}
}

// getHistory returns every record of one environment in registration order,
// oldest first, so callers can trace rollback positions back from the current
// release point.
func getHistory(deps Dependencies) gin.HandlerFunc {
	return func(c *gin.Context) {
		environment := c.Param("environment")
		if !environmentExists(c, deps, environment) {
			return
		}
		releases, err := deps.Store.ListReleases(store.ReleaseFilter{Environment: environment})
		if err != nil {
			fail(c, http.StatusInternalServerError, store.CodeStorageUnavailable, "database is not available")
			return
		}
		c.JSON(http.StatusOK, gin.H{"environment": environment, "releases": toReleaseList(releases)})
	}
}

// environmentExists writes the environment_not_found error and returns false
// when the environment has no records at all.
func environmentExists(c *gin.Context, deps Dependencies, environment string) bool {
	exists, err := deps.Store.EnvironmentExists(environment)
	if err != nil {
		fail(c, http.StatusInternalServerError, store.CodeStorageUnavailable, "database is not available")
		return false
	}
	if !exists {
		fail(c, http.StatusNotFound, store.CodeEnvironmentNotFound, "no releases recorded for environment "+environment)
		return false
	}
	return true
}
