// Promotion routes name an ordered sequence of registered environments so a
// release batch can be promoted along a stable, reusable line. Routes are
// immutable facts: creation is idempotent for an identical sequence and a
// different sequence under the same name is a conflict. Batches bind to at
// most one route; chain and trace queries fall back to the bound route when
// the request carries no explicit selector.
package v1

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/luwa07832/deployment-tracker/internal/store"
)

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

// createPromotionRoute answers POST /api/v1/promotion-routes. Re-creating a
// route with the identical sequence returns the existing route with 200.
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
		name, environments, ok := validatePromotionRouteInput(c, &input)
		if !ok {
			return
		}
		unknown := []string{}
		for _, key := range environments {
			exists, err := deps.Store.TrackedEnvironmentExists(key)
			if err != nil {
				failStorage(c)
				return
			}
			if !exists {
				unknown = append(unknown, key)
			}
		}
		if len(unknown) > 0 {
			sort.Strings(unknown)
			fail(c, http.StatusNotFound, store.CodeEnvironmentNotFoundV1,
				"unknown environments: "+strings.Join(uniqueSorted(unknown), ", "))
			return
		}
		existing, err := deps.Store.GetPromotionRoute(name)
		if err != nil {
			failStorage(c)
			return
		}
		if existing != nil {
			if stringSlicesEqual(existing.Environments, environments) {
				c.JSON(http.StatusOK, gin.H{"promotion_route": toPromotionRouteView(existing)})
				return
			}
			fail(c, http.StatusConflict, store.CodePromotionRouteConflictV1,
				"promotion route "+name+" already exists with a different environment sequence")
			return
		}
		route := &store.PromotionRoute{Name: name, Environments: environments}
		if err := deps.Store.InsertPromotionRoute(route); err != nil {
			failStorage(c)
			return
		}
		c.JSON(http.StatusCreated, gin.H{"promotion_route": toPromotionRouteView(route)})
	}
}

// validatePromotionRouteInput applies the field rules for route creation and
// returns the trimmed name plus the ordered, duplicate-free environment
// sequence. On failure it writes the canonical 422 response.
func validatePromotionRouteInput(c *gin.Context, input *createPromotionRouteInput) (string, []string, bool) {
	if input.Name == nil || strings.TrimSpace(*input.Name) == "" {
		fail(c, http.StatusUnprocessableEntity, store.CodePromotionRouteValidationV1, "name is required")
		return "", nil, false
	}
	name := strings.TrimSpace(*input.Name)
	if len(name) > 128 {
		fail(c, http.StatusUnprocessableEntity, store.CodePromotionRouteValidationV1,
			"name must be at most 128 characters")
		return "", nil, false
	}
	if input.Environments == nil || len(*input.Environments) == 0 {
		fail(c, http.StatusUnprocessableEntity, store.CodePromotionRouteValidationV1,
			"environments is required and must be a non-empty ordered list")
		return "", nil, false
	}
	environments := make([]string, 0, len(*input.Environments))
	seen := map[string]bool{}
	duplicates := []string{}
	for _, raw := range *input.Environments {
		key := strings.TrimSpace(raw)
		if key == "" {
			fail(c, http.StatusUnprocessableEntity, store.CodePromotionRouteValidationV1,
				"environments must not contain empty entries")
			return "", nil, false
		}
		if seen[key] {
			duplicates = append(duplicates, key)
		}
		seen[key] = true
		environments = append(environments, key)
	}
	if len(duplicates) > 0 {
		sort.Strings(duplicates)
		fail(c, http.StatusUnprocessableEntity, store.CodePromotionRouteValidationV1,
			"environments contains repeated environments: "+strings.Join(uniqueSorted(duplicates), ", "))
		return "", nil, false
	}
	return name, environments, true
}

// stringSlicesEqual reports whether two ordered sequences hold the same
// elements in the same order.
func stringSlicesEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

// listPromotionRoutes answers GET /api/v1/promotion-routes with every route
// ordered by name.
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

// getPromotionRoute answers GET /api/v1/promotion-routes/:name. Blank or
// unknown names are both PROMOTION_ROUTE_NOT_FOUND.
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
			fail(c, http.StatusNotFound, store.CodePromotionRouteNotFoundV1,
				"no such promotion route "+name)
			return
		}
		c.JSON(http.StatusOK, gin.H{"promotion_route": toPromotionRouteView(route)})
	}
}

type bindPromotionRouteInput struct {
	Route *string `json:"route"`
}

// bindPromotionRoute answers PUT
// /api/v1/release-batches/:batch_id/promotion-route. The first binding
// returns 201; repeating the same route is idempotent (200); binding a
// different route conflicts.
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
		if input.Route == nil || strings.TrimSpace(*input.Route) == "" {
			fail(c, http.StatusUnprocessableEntity, store.CodePromotionRouteBindingValidationV1,
				"route is required")
			return
		}
		routeName := strings.TrimSpace(*input.Route)
		route, err := deps.Store.GetPromotionRoute(routeName)
		if err != nil {
			failStorage(c)
			return
		}
		if route == nil {
			fail(c, http.StatusNotFound, store.CodePromotionRouteNotFoundV1,
				"no such promotion route "+routeName)
			return
		}
		batchID := strings.TrimSpace(c.Param("batch_id"))
		if !requireBatch(c, deps, batchID) {
			return
		}
		bound, found, err := deps.Store.BatchPromotionRoute(batchID)
		if err != nil {
			failStorage(c)
			return
		}
		if found {
			if bound == routeName {
				c.JSON(http.StatusOK, gin.H{"batch_id": batchID, "route": routeName})
				return
			}
			fail(c, http.StatusConflict, store.CodePromotionRouteAlreadyBoundV1,
				"release batch "+batchID+" is already bound to promotion route "+bound)
			return
		}
		if err := deps.Store.BindBatchPromotionRoute(batchID, routeName); err != nil {
			failStorage(c)
			return
		}
		c.JSON(http.StatusCreated, gin.H{"batch_id": batchID, "route": routeName})
	}
}

// resolvePromotionSequence determines the ordered environment sequence for
// the chain and trace queries. An explicit route query parameter wins over
// the environments parameter is a conflict; with neither selector the batch's
// bound route supplies the sequence. Selector validation and route/binding
// reads all happen before the batch existence check.
func resolvePromotionSequence(c *gin.Context, deps Dependencies, batchID string) ([]string, bool) {
	routeName := strings.TrimSpace(c.Query("route"))
	rawEnvironments := strings.TrimSpace(c.Query("environments"))
	if routeName != "" && rawEnvironments != "" {
		fail(c, http.StatusBadRequest, store.CodePromotionSelectorConflictV1,
			"route and environments selectors must not be combined")
		return nil, false
	}
	if rawEnvironments != "" {
		return parseEnvironmentSequence(c, deps, rawEnvironments)
	}
	if routeName != "" {
		route, err := deps.Store.GetPromotionRoute(routeName)
		if err != nil {
			failStorage(c)
			return nil, false
		}
		if route == nil {
			fail(c, http.StatusNotFound, store.CodePromotionRouteNotFoundV1,
				"no such promotion route "+routeName)
			return nil, false
		}
		return route.Environments, true
	}
	bound, found, err := deps.Store.BatchPromotionRoute(batchID)
	if err != nil {
		failStorage(c)
		return nil, false
	}
	if found {
		route, err := deps.Store.GetPromotionRoute(bound)
		if err != nil {
			failStorage(c)
			return nil, false
		}
		if route != nil {
			return route.Environments, true
		}
	}
	fail(c, http.StatusBadRequest, store.CodePromotionSequenceInvalidV1,
		"environments query parameter is required and must be an ordered, comma-separated list")
	return nil, false
}
