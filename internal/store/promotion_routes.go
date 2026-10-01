package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// Error codes for promotion route registration and batch binding. Values are
// stable public identifiers returned verbatim in error responses.
const (
	CodePromotionRouteNotFoundV1          = "PROMOTION_ROUTE_NOT_FOUND"
	CodePromotionRouteConflictV1          = "PROMOTION_ROUTE_CONFLICT"
	CodePromotionRouteValidationV1        = "PROMOTION_ROUTE_VALIDATION_FAILED"
	CodePromotionRouteAlreadyBoundV1      = "PROMOTION_ROUTE_ALREADY_BOUND"
	CodePromotionRouteBindingValidationV1 = "PROMOTION_ROUTE_BINDING_VALIDATION_FAILED"
	CodePromotionSelectorConflictV1       = "PROMOTION_SELECTOR_CONFLICT"
)

// PromotionRoute is a named, ordered sequence of registered environments
// that release batches can be promoted along.
type PromotionRoute struct {
	ID           int64
	Name         string
	Environments []string
}

// ErrPromotionRouteConflict marks a re-registration of an existing route name
// whose environment sequence differs from the stored one.
type ErrPromotionRouteConflict struct {
	Existing *PromotionRoute
}

func (e *ErrPromotionRouteConflict) Error() string {
	return "promotion route " + e.Existing.Name + " is already registered with a different environment sequence"
}

// ErrPromotionRouteAlreadyBound marks an attempt to rebind a batch that is
// already bound to a different route.
type ErrPromotionRouteAlreadyBound struct {
	BatchID   string
	RouteName string
}

func (e *ErrPromotionRouteAlreadyBound) Error() string {
	return "release batch " + e.BatchID + " is already bound to promotion route " + e.RouteName
}

// sqlQuerier is satisfied by both *sql.DB and *sql.Tx.
type sqlQuerier interface {
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// CreatePromotionRoute stores a named route and its ordered environments.
// Re-submitting the same name with the same sequence is idempotent: the
// stored route is returned unchanged and created is false. A same-name
// request carrying a different sequence fails with
// *ErrPromotionRouteConflict.
func (s *Store) CreatePromotionRoute(route *PromotionRoute) (created bool, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("store: create promotion route: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var existingID int64
	err = tx.QueryRow(`SELECT id FROM promotion_routes WHERE name = ?`, route.Name).Scan(&existingID)
	switch {
	case err == nil:
		existing, err := scanPromotionRoute(tx, existingID)
		if err != nil {
			return false, err
		}
		if !equalStringSlices(existing.Environments, route.Environments) {
			return false, &ErrPromotionRouteConflict{Existing: existing}
		}
		*route = *existing
		return false, nil
	case !errors.Is(err, sql.ErrNoRows):
		return false, fmt.Errorf("store: create promotion route: %w", err)
	}

	res, err := tx.Exec(`INSERT INTO promotion_routes (name) VALUES (?)`, route.Name)
	if err != nil {
		return false, fmt.Errorf("store: create promotion route: %w", err)
	}
	routeID, err := res.LastInsertId()
	if err != nil {
		return false, fmt.Errorf("store: create promotion route: %w", err)
	}
	for position, environment := range route.Environments {
		if _, err := tx.Exec(
			`INSERT INTO promotion_route_environments (route_id, position, environment) VALUES (?, ?, ?)`,
			routeID, position, environment,
		); err != nil {
			return false, fmt.Errorf("store: create promotion route: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("store: create promotion route: %w", err)
	}
	stored, err := s.GetPromotionRoute(route.Name)
	if err != nil {
		return false, err
	}
	*route = *stored
	return true, nil
}

// GetPromotionRoute returns one route by its exact name, or (nil, nil) when
// no route with that name exists. Names are never folded or fuzzy-matched.
func (s *Store) GetPromotionRoute(name string) (*PromotionRoute, error) {
	var id int64
	err := s.db.QueryRow(`SELECT id FROM promotion_routes WHERE name = ?`, name).Scan(&id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: get promotion route: %w", err)
	}
	return scanPromotionRoute(s.db, id)
}

// ListPromotionRoutes returns every route ordered by name, each with its
// environments in promotion order.
func (s *Store) ListPromotionRoutes() ([]PromotionRoute, error) {
	rows, err := s.db.Query(
		`SELECT r.id, r.name, e.environment
		 FROM promotion_routes r
		 JOIN promotion_route_environments e ON e.route_id = r.id
		 ORDER BY r.name ASC, e.position ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list promotion routes: %w", err)
	}
	defer rows.Close()
	routes := []PromotionRoute{}
	indexByID := map[int64]int{}
	for rows.Next() {
		var route PromotionRoute
		var environment string
		if err := rows.Scan(&route.ID, &route.Name, &environment); err != nil {
			return nil, fmt.Errorf("store: list promotion routes: %w", err)
		}
		position, seen := indexByID[route.ID]
		if !seen {
			route.Environments = []string{}
			routes = append(routes, route)
			position = len(routes) - 1
			indexByID[route.ID] = position
		}
		routes[position].Environments = append(routes[position].Environments, environment)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list promotion routes: %w", err)
	}
	return routes, nil
}

// BindReleaseBatchRoute binds a batch to a route. Binding the same route
// again is idempotent (created=false); binding a different route fails with
// *ErrPromotionRouteAlreadyBound.
func (s *Store) BindReleaseBatchRoute(batchID string, route *PromotionRoute) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("store: bind promotion route: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var boundRouteID int64
	err = tx.QueryRow(`SELECT route_id FROM release_batch_routes WHERE batch_id = ?`, batchID).Scan(&boundRouteID)
	switch {
	case err == nil:
		if boundRouteID == route.ID {
			return false, nil
		}
		boundName, nameErr := promotionRouteNameByID(tx, boundRouteID)
		if nameErr != nil {
			return false, nameErr
		}
		return false, &ErrPromotionRouteAlreadyBound{BatchID: batchID, RouteName: boundName}
	case !errors.Is(err, sql.ErrNoRows):
		return false, fmt.Errorf("store: bind promotion route: %w", err)
	}

	if _, err := tx.Exec(
		`INSERT INTO release_batch_routes (batch_id, route_id) VALUES (?, ?)`,
		batchID, route.ID,
	); err != nil {
		return false, fmt.Errorf("store: bind promotion route: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("store: bind promotion route: %w", err)
	}
	return true, nil
}

// GetBoundRouteName returns the exact route name bound to a batch, or "" when
// the batch has no binding.
func (s *Store) GetBoundRouteName(batchID string) (string, error) {
	var name string
	err := s.db.QueryRow(
		`SELECT r.name FROM release_batch_routes b
		 JOIN promotion_routes r ON r.id = b.route_id
		 WHERE b.batch_id = ?`,
		batchID,
	).Scan(&name)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("store: get bound promotion route: %w", err)
	}
	return name, nil
}

// scanPromotionRoute loads one route and its ordered environments.
func scanPromotionRoute(q sqlQuerier, id int64) (*PromotionRoute, error) {
	route := &PromotionRoute{ID: id, Environments: []string{}}
	if err := q.QueryRow(`SELECT name FROM promotion_routes WHERE id = ?`, id).Scan(&route.Name); err != nil {
		return nil, fmt.Errorf("store: scan promotion route: %w", err)
	}
	rows, err := q.Query(
		`SELECT environment FROM promotion_route_environments WHERE route_id = ? ORDER BY position ASC`,
		id,
	)
	if err != nil {
		return nil, fmt.Errorf("store: scan promotion route: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var environment string
		if err := rows.Scan(&environment); err != nil {
			return nil, fmt.Errorf("store: scan promotion route: %w", err)
		}
		route.Environments = append(route.Environments, environment)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: scan promotion route: %w", err)
	}
	return route, nil
}

func promotionRouteNameByID(q sqlQuerier, id int64) (string, error) {
	var name string
	if err := q.QueryRow(`SELECT name FROM promotion_routes WHERE id = ?`, id).Scan(&name); err != nil {
		return "", fmt.Errorf("store: bound promotion route name: %w", err)
	}
	return name, nil
}

func equalStringSlices(left, right []string) bool {
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
