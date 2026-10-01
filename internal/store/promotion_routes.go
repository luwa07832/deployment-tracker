package store

import (
	"database/sql"
	"fmt"
)

// PromotionRoute is a named, ordered sequence of registered environment keys
// that release batches can be promoted along. Routes are immutable once
// created: the same name can never be re-created with a different sequence.
type PromotionRoute struct {
	Name         string
	Environments []string
}

// InsertPromotionRoute stores one promotion route with its ordered
// environment sequence. The name must be unique; callers check for an
// existing route first so idempotent re-creation stays a read.
func (s *Store) InsertPromotionRoute(route *PromotionRoute) error {
	res, err := s.db.Exec(`INSERT INTO promotion_routes (name) VALUES (?)`, route.Name)
	if err != nil {
		return fmt.Errorf("store: insert promotion route: %w", err)
	}
	routeID, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("store: insert promotion route: %w", err)
	}
	for position, environment := range route.Environments {
		if _, err := s.db.Exec(
			`INSERT INTO promotion_route_environments (route_id, position, environment) VALUES (?, ?, ?)`,
			routeID, position, environment,
		); err != nil {
			return fmt.Errorf("store: insert promotion route environment: %w", err)
		}
	}
	return nil
}

// GetPromotionRoute returns one route by its exact name, or (nil, nil) when
// the name is unknown. Matching is case-sensitive: names are never folded or
// fuzzily resolved.
func (s *Store) GetPromotionRoute(name string) (*PromotionRoute, error) {
	row := s.db.QueryRow(`SELECT id FROM promotion_routes WHERE name = ?`, name)
	var routeID int64
	if err := row.Scan(&routeID); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("store: get promotion route: %w", err)
	}
	environments, err := s.routeEnvironments(routeID)
	if err != nil {
		return nil, err
	}
	return &PromotionRoute{Name: name, Environments: environments}, nil
}

// ListPromotionRoutes returns every route ordered by name ascending.
func (s *Store) ListPromotionRoutes() ([]PromotionRoute, error) {
	rows, err := s.db.Query(`SELECT id, name FROM promotion_routes ORDER BY name ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: list promotion routes: %w", err)
	}
	defer rows.Close()
	type routeRow struct {
		id   int64
		name string
	}
	routeRows := []routeRow{}
	for rows.Next() {
		var row routeRow
		if err := rows.Scan(&row.id, &row.name); err != nil {
			return nil, fmt.Errorf("store: list promotion routes: %w", err)
		}
		routeRows = append(routeRows, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list promotion routes: %w", err)
	}
	routes := make([]PromotionRoute, 0, len(routeRows))
	for _, row := range routeRows {
		environments, err := s.routeEnvironments(row.id)
		if err != nil {
			return nil, err
		}
		routes = append(routes, PromotionRoute{Name: row.name, Environments: environments})
	}
	return routes, nil
}

// routeEnvironments returns the ordered environment sequence of one route.
func (s *Store) routeEnvironments(routeID int64) ([]string, error) {
	rows, err := s.db.Query(
		`SELECT environment FROM promotion_route_environments WHERE route_id = ? ORDER BY position ASC`,
		routeID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list promotion route environments: %w", err)
	}
	defer rows.Close()
	environments := []string{}
	for rows.Next() {
		var environment string
		if err := rows.Scan(&environment); err != nil {
			return nil, fmt.Errorf("store: list promotion route environments: %w", err)
		}
		environments = append(environments, environment)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list promotion route environments: %w", err)
	}
	return environments, nil
}

// BindBatchPromotionRoute pins a release batch to a promotion route. The
// binding is written once and never updated; re-binding attempts are rejected
// by the caller before reaching the store.
func (s *Store) BindBatchPromotionRoute(batchID, routeName string) error {
	if _, err := s.db.Exec(
		`INSERT INTO release_batch_promotion_routes (batch_id, route_name) VALUES (?, ?)`,
		batchID, routeName,
	); err != nil {
		return fmt.Errorf("store: bind batch promotion route: %w", err)
	}
	return nil
}

// BatchPromotionRoute returns the route name bound to a batch, or found=false
// when the batch has no binding.
func (s *Store) BatchPromotionRoute(batchID string) (routeName string, found bool, err error) {
	row := s.db.QueryRow(
		`SELECT route_name FROM release_batch_promotion_routes WHERE batch_id = ?`, batchID,
	)
	if err := row.Scan(&routeName); err != nil {
		if err == sql.ErrNoRows {
			return "", false, nil
		}
		return "", false, fmt.Errorf("store: get batch promotion route: %w", err)
	}
	return routeName, true, nil
}
