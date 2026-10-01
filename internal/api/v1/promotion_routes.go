package v1

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/luwa07832/deployment-tracker/internal/store"
)

const maxPromotionRouteNameLength = 128

type promotionRouteView struct {
	Name         string   `json:"name"`
	Environments []string `json:"environments"`
}

func toPromotionRouteView(route *store.PromotionRoute) promotionRouteView {
	environments := make([]string, 0, len(route.Environments))
	environments = append(environments, route.Environments...)
	return promotionRouteView{Name: route.Name, Environments: environments}
}

type createPromotionRouteInput struct {
	Name         *string   `json:"name"`
	Environments *[]string `json:"environments"`
}

// createPromotionRoute answers POST /api/v1/promotion-routes. The first
// registration of a name returns 201; repeating the exact same ordered
// sequence returns the stored route with 200; a same-name request carrying a
// different sequence conflicts.
func createPromotionRoute(deps Dependencies) gin.HandlerFunc {
	return func(c *gin.Context) {
		var input createPromotionRouteInput
		if err := json.NewDecoder(c.Request.Body).Decode(&input); err != nil {
			var typeErr *json.UnmarshalTypeError
			if errors.As(err, &typeErr) {
				fail(c, http.StatusUnprocessableEntity, store.CodePromotionRouteValidationV1,
					"field "+typeErr.Field+" has an invalid type")
				return
			}
			fail(c, http.StatusBadRequest, store.CodeInvalidRequest, "request body must be valid JSON")
			return
		}
		name, environments, problem := validatePromotionRouteInput(&input)
		if problem != "" {
			fail(c, http.StatusUnprocessableEntity, store.CodePromotionRouteValidationV1, problem)
			return
		}
		unknownEnvironment, err := firstUnknownEnvironment(deps, environments)
		if err != nil {
			failStorage(c)
			return
		}
		if unknownEnvironment != "" {
			fail(c, http.StatusNotFound, store.CodeEnvironmentNotFoundV1,
				"no such environment "+unknownEnvironment)
			return
		}
		route := &store.PromotionRoute{Name: name, Environments: environments}
		created, err := deps.Store.CreatePromotionRoute(route)
		if err != nil {
			var conflict *store.ErrPromotionRouteConflict
			if errors.As(err, &conflict) {
				fail(c, http.StatusConflict, store.CodePromotionRouteConflictV1,
					"promotion route "+name+" is already registered with a different environment sequence")
				return
			}
			failStorage(c)
			return
		}
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		c.JSON(status, gin.H{"promotion_route": toPromotionRouteView(route)})
	}
}

// validatePromotionRouteInput applies the field rules: name is trimmed and
// must be non-empty and at most 128 characters; environments is an ordered
// list with no blank or repeated entries.
func validatePromotionRouteInput(input *createPromotionRouteInput) (string, []string, string) {
	if input.Name == nil {
		return "", nil, "name is required"
	}
	name := strings.TrimSpace(*input.Name)
	if name == "" {
		return "", nil, "name is required"
	}
	if len(name) > maxPromotionRouteNameLength {
		return "", nil, "name must be at most 128 characters"
	}
	if input.Environments == nil {
		return "", nil, "environments is required"
	}
	rawEnvironments := *input.Environments
	if len(rawEnvironments) == 0 {
		return "", nil, "environments must contain at least one environment"
	}
	environments := make([]string, 0, len(rawEnvironments))
	seen := map[string]bool{}
	for _, raw := range rawEnvironments {
		key := strings.TrimSpace(raw)
		if key == "" {
			return "", nil, "environments must not contain blank environments"
		}
		if seen[key] {
			return "", nil, "environments must not contain repeated environments: " + key
		}
		seen[key] = true
		environments = append(environments, key)
	}
	return name, environments, ""
}

// firstUnknownEnvironment returns the first (in sequence order) environment
// that is not registered, or "" when all exist.
func firstUnknownEnvironment(deps Dependencies, environments []string) (string, error) {
	for _, key := range environments {
		exists, err := deps.Store.TrackedEnvironmentExists(key)
		if err != nil {
			return "", err
		}
		if !exists {
			return key, nil
		}
	}
	return "", nil
}

// listPromotionRoutes answers GET /api/v1/promotion-routes, ordered by name.
func listPromotionRoutes(deps Dependencies) gin.HandlerFunc {
	return func(c *gin.Context) {
		routes, err := deps.Store.ListPromotionRoutes()
		if err != nil {
			failStorage(c)
			return
		}
		views := make([]promotionRouteView, 0, len(routes))
		for i := range routes {
			views = append(views, toPromotionRouteView(&routes[i]))
		}
		c.JSON(http.StatusOK, gin.H{"promotion_routes": views})
	}
}

// getPromotionRoute answers GET /api/v1/promotion-routes/:name with exact
// name matching; a blank or unknown name is a NotFound.
func getPromotionRoute(deps Dependencies) gin.HandlerFunc {
	return func(c *gin.Context) {
		name := strings.TrimSpace(c.Param("name"))
		if name == "" {
			fail(c, http.StatusNotFound, store.CodePromotionRouteNotFoundV1, "no such promotion route")
			return
		}
		route, err := deps.Store.GetPromotionRoute(name)
		if err != nil {
			failStorage(c)
			return
		}
		if route == nil {
			fail(c, http.StatusNotFound, store.CodePromotionRouteNotFoundV1, "no such promotion route "+name)
			return
		}
		c.JSON(http.StatusOK, gin.H{"promotion_route": toPromotionRouteView(route)})
	}
}

type bindPromotionRouteInput struct {
	Route *string `json:"route"`
}

type promotionRouteBindingView struct {
	BatchID string `json:"batch_id"`
	Route   string `json:"route"`
}

// bindPromotionRoute answers PUT
// /api/v1/release-batches/:batch_id/promotion-route. The first binding
// returns 201; re-binding the same route returns 200; switching a bound
// batch to another route conflicts.
func bindPromotionRoute(deps Dependencies) gin.HandlerFunc {
	return func(c *gin.Context) {
		var input bindPromotionRouteInput
		if err := json.NewDecoder(c.Request.Body).Decode(&input); err != nil {
			var typeErr *json.UnmarshalTypeError
			if errors.As(err, &typeErr) {
				fail(c, http.StatusUnprocessableEntity, store.CodePromotionRouteBindingValidationV1,
					"field "+typeErr.Field+" has an invalid type")
				return
			}
			fail(c, http.StatusBadRequest, store.CodeInvalidRequest, "request body must be valid JSON")
			return
		}
		if input.Route == nil {
			fail(c, http.StatusUnprocessableEntity, store.CodePromotionRouteBindingValidationV1, "route is required")
			return
		}
		routeName := strings.TrimSpace(*input.Route)
		if routeName == "" {
			fail(c, http.StatusUnprocessableEntity, store.CodePromotionRouteBindingValidationV1, "route is required")
			return
		}
		route, err := deps.Store.GetPromotionRoute(routeName)
		if err != nil {
			failStorage(c)
			return
		}
		if route == nil {
			fail(c, http.StatusNotFound, store.CodePromotionRouteNotFoundV1, "no such promotion route "+routeName)
			return
		}
		batchID := strings.TrimSpace(c.Param("batch_id"))
		if !requireBatch(c, deps, batchID) {
			return
		}
		created, err := deps.Store.BindReleaseBatchRoute(batchID, route)
		if err != nil {
			var alreadyBound *store.ErrPromotionRouteAlreadyBound
			if errors.As(err, &alreadyBound) {
				fail(c, http.StatusConflict, store.CodePromotionRouteAlreadyBoundV1,
					"release batch "+batchID+" is already bound to promotion route "+alreadyBound.RouteName)
				return
			}
			failStorage(c)
			return
		}
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		c.JSON(status, promotionRouteBindingView{BatchID: batchID, Route: route.Name})
	}
}
