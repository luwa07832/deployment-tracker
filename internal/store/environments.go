package store

import (
	"database/sql"
	"fmt"
)

// Error codes introduced for the traceable release-record API. Values are
// stable public identifiers; they are returned verbatim in error responses.
const (
	CodeEnvironmentNotFoundV1    = "ENVIRONMENT_NOT_FOUND"
	CodeReleaseAlreadyExistsV1   = "RELEASE_ALREADY_EXISTS"
	CodeReleaseValidationV1      = "RELEASE_VALIDATION_FAILED"
	CodeSameEnvironmentCompareV1 = "SAME_ENVIRONMENT_COMPARE"
	CodeInvalidCompareRangeV1    = "INVALID_COMPARE_RANGE"
	CodeReleaseVersionNotFoundV1 = "RELEASE_VERSION_NOT_FOUND"
	CodeReleaseRecordNotFoundV1  = "RELEASE_RECORD_NOT_FOUND"
	CodeEnvironmentValidationV1  = "ENVIRONMENT_VALIDATION_FAILED"

	// Release promotion chain API (batch-scoped).
	CodeReleaseBatchNotFoundV1     = "RELEASE_BATCH_NOT_FOUND"
	CodePromotionSequenceInvalidV1 = "INVALID_PROMOTION_SEQUENCE"
	CodePromotionOrderConflictV1   = "PROMOTION_ORDER_CONFLICT"
	CodeSamePromotionNodeV1        = "SAME_PROMOTION_NODE"
	CodePromotionChangeNotFoundV1  = "PROMOTION_CHANGE_NOT_FOUND"

	// Named promotion routes and per-batch route bindings.
	CodePromotionRouteNotFoundV1          = "PROMOTION_ROUTE_NOT_FOUND"
	CodePromotionRouteConflictV1          = "PROMOTION_ROUTE_CONFLICT"
	CodePromotionRouteValidationV1        = "PROMOTION_ROUTE_VALIDATION_FAILED"
	CodePromotionRouteAlreadyBoundV1      = "PROMOTION_ROUTE_ALREADY_BOUND"
	CodePromotionRouteBindingValidationV1 = "PROMOTION_ROUTE_BINDING_VALIDATION_FAILED"
	CodePromotionSelectorConflictV1       = "PROMOTION_SELECTOR_CONFLICT"

	// Cross-environment release comparison with explicitly selected versions
	// and per-environment release history.
	CodeReleaseComparisonEnvironmentNotFound = "ReleaseComparisonEnvironmentNotFound"
	CodeReleaseComparisonVersionNotFound     = "ReleaseComparisonVersionNotFound"
	CodeReleaseComparisonDataIncomplete      = "ReleaseComparisonDataIncomplete"
	CodeInvalidReleaseComparisonQuery        = "INVALID_RELEASE_COMPARISON_QUERY"
	CodeInvalidHistoryPagination             = "INVALID_HISTORY_PAGINATION"
)

// TrackedEnvironment is an environment that may receive release records.
type TrackedEnvironment struct {
	Environment  string
	DisplayName  string
	RegisteredAt string
}

// EnsureEnvironment registers an environment. It is idempotent: an existing
// environment is returned unchanged. New rows gain a server-generated
// registered_at timestamp.
func (s *Store) EnsureEnvironment(env *TrackedEnvironment) (created bool, err error) {
	res, err := s.db.Exec(
		`INSERT OR IGNORE INTO tracked_environments (environment, display_name) VALUES (?, ?)`,
		env.Environment, env.DisplayName,
	)
	if err != nil {
		return false, fmt.Errorf("store: ensure environment: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: ensure environment: %w", err)
	}
	created = affected > 0
	stored, err := s.GetEnvironment(env.Environment)
	if err != nil {
		return false, err
	}
	*env = *stored
	return created, nil
}

// GetEnvironment returns one registered environment, or (nil, nil) when the
// key is unknown.
func (s *Store) GetEnvironment(environment string) (*TrackedEnvironment, error) {
	row := s.db.QueryRow(
		`SELECT environment, display_name, registered_at FROM tracked_environments WHERE environment = ?`,
		environment,
	)
	var env TrackedEnvironment
	if err := row.Scan(&env.Environment, &env.DisplayName, &env.RegisteredAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("store: get environment: %w", err)
	}
	return &env, nil
}

// TrackedEnvironmentExists reports whether the environment key is registered.
func (s *Store) TrackedEnvironmentExists(environment string) (bool, error) {
	var count int
	if err := s.db.QueryRow(
		`SELECT count(1) FROM tracked_environments WHERE environment = ?`, environment,
	).Scan(&count); err != nil {
		return false, fmt.Errorf("store: check tracked environment: %w", err)
	}
	return count > 0, nil
}

// ListEnvironments returns every registered environment ordered by key.
func (s *Store) ListEnvironments() ([]TrackedEnvironment, error) {
	rows, err := s.db.Query(
		`SELECT environment, display_name, registered_at FROM tracked_environments ORDER BY environment ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list environments: %w", err)
	}
	defer rows.Close()
	environments := []TrackedEnvironment{}
	for rows.Next() {
		var env TrackedEnvironment
		if err := rows.Scan(&env.Environment, &env.DisplayName, &env.RegisteredAt); err != nil {
			return nil, fmt.Errorf("store: list environments: %w", err)
		}
		environments = append(environments, env)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list environments: %w", err)
	}
	return environments, nil
}
