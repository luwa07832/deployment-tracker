// Package v1 holds the traceable release-record API introduced after the
// baseline deployment API. It shares the same error envelope and JSON naming
// style but keeps its own routes under /api/v1 so baseline responses stay
// byte-compatible.
package v1

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/luwa07832/deployment-tracker/internal/store"
)

// fail writes the single documented error shape shared with the baseline API.
func fail(c *gin.Context, status int, code, message string) {
	c.AbortWithStatusJSON(status, gin.H{"error": gin.H{"code": code, "message": message}})
}

type environmentView struct {
	Environment  string `json:"environment"`
	DisplayName  string `json:"display_name"`
	RegisteredAt string `json:"registered_at"`
}

func toEnvironmentView(env *store.TrackedEnvironment) environmentView {
	return environmentView{
		Environment:  env.Environment,
		DisplayName:  env.DisplayName,
		RegisteredAt: env.RegisteredAt,
	}
}

type registerEnvironmentInput struct {
	Environment *string `json:"environment"`
	DisplayName *string `json:"display_name"`
}

// registerEnvironment creates an environment key. Registration is idempotent:
// repeating an existing key returns it with HTTP 200 instead of failing.
func registerEnvironment(deps Dependencies) gin.HandlerFunc {
	return func(c *gin.Context) {
		var input registerEnvironmentInput
		if err := json.NewDecoder(c.Request.Body).Decode(&input); err != nil {
			var typeErr *json.UnmarshalTypeError
			if errors.As(err, &typeErr) {
				fail(c, http.StatusUnprocessableEntity, store.CodeEnvironmentValidationV1,
					"field "+typeErr.Field+" has an invalid type")
				return
			}
			fail(c, http.StatusBadRequest, store.CodeInvalidRequest, "request body must be valid JSON")
			return
		}
		if input.Environment == nil || strings.TrimSpace(*input.Environment) == "" {
			fail(c, http.StatusUnprocessableEntity, store.CodeEnvironmentValidationV1, "environment is required")
			return
		}
		key := strings.TrimSpace(*input.Environment)
		if len(key) > 128 {
			fail(c, http.StatusUnprocessableEntity, store.CodeEnvironmentValidationV1,
				"environment must be at most 128 characters")
			return
		}
		displayName := ""
		if input.DisplayName != nil {
			displayName = strings.TrimSpace(*input.DisplayName)
		}
		env := &store.TrackedEnvironment{Environment: key, DisplayName: displayName}
		created, err := deps.Store.EnsureEnvironment(env)
		if err != nil {
			fail(c, http.StatusInternalServerError, store.CodeStorageUnavailable, "database is not available")
			return
		}
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		c.JSON(status, gin.H{"environment": toEnvironmentView(env)})
	}
}

// listEnvironments returns every registered environment key.
func listEnvironments(deps Dependencies) gin.HandlerFunc {
	return func(c *gin.Context) {
		environments, err := deps.Store.ListEnvironments()
		if err != nil {
			fail(c, http.StatusInternalServerError, store.CodeStorageUnavailable, "database is not available")
			return
		}
		views := make([]environmentView, 0, len(environments))
		for i := range environments {
			views = append(views, toEnvironmentView(&environments[i]))
		}
		c.JSON(http.StatusOK, gin.H{"environments": views})
	}
}

// getEnvironment returns one environment, or ENVIRONMENT_NOT_FOUND.
func getEnvironment(deps Dependencies) gin.HandlerFunc {
	return func(c *gin.Context) {
		key := c.Param("environment")
		env, err := deps.Store.GetEnvironment(key)
		if err != nil {
			fail(c, http.StatusInternalServerError, store.CodeStorageUnavailable, "database is not available")
			return
		}
		if env == nil {
			fail(c, http.StatusNotFound, store.CodeEnvironmentNotFoundV1, "no such environment "+key)
			return
		}
		c.JSON(http.StatusOK, gin.H{"environment": toEnvironmentView(env)})
	}
}

// requireEnvironment writes ENVIRONMENT_NOT_FOUND and returns false when the
// target environment is not registered.
func requireEnvironment(c *gin.Context, deps Dependencies, key string) bool {
	exists, err := deps.Store.TrackedEnvironmentExists(key)
	if err != nil {
		fail(c, http.StatusInternalServerError, store.CodeStorageUnavailable, "database is not available")
		return false
	}
	if !exists {
		fail(c, http.StatusNotFound, store.CodeEnvironmentNotFoundV1, "no such environment "+key)
		return false
	}
	return true
}
