package store

import (
	"errors"
	"testing"
)

func TestPromotionRouteCreateIdempotentAndConflict(t *testing.T) {
	db := openTestStore(t)
	for _, env := range []string{"dev", "test", "prod"} {
		if _, err := db.EnsureEnvironment(&TrackedEnvironment{Environment: env}); err != nil {
			t.Fatalf("ensure %s: %v", env, err)
		}
	}

	route := &PromotionRoute{Name: "prod-line", Environments: []string{"dev", "test", "prod"}}
	created, err := db.CreatePromotionRoute(route)
	if err != nil || !created {
		t.Fatalf("first create = %v, %v", created, err)
	}
	if route.ID == 0 || len(route.Environments) != 3 {
		t.Fatalf("stored route = %+v", route)
	}

	// Same sequence: idempotent, not created.
	same := &PromotionRoute{Name: "prod-line", Environments: []string{"dev", "test", "prod"}}
	created, err = db.CreatePromotionRoute(same)
	if err != nil || created {
		t.Fatalf("same sequence = %v, %v", created, err)
	}
	if same.ID != route.ID {
		t.Fatalf("idempotent create returned a different row: %d != %d", same.ID, route.ID)
	}

	// Different sequence: conflict carrying the existing route.
	different := &PromotionRoute{Name: "prod-line", Environments: []string{"dev", "prod"}}
	_, err = db.CreatePromotionRoute(different)
	var conflict *ErrPromotionRouteConflict
	if !errors.As(err, &conflict) {
		t.Fatalf("different sequence err = %v, want *ErrPromotionRouteConflict", err)
	}
	if got := conflict.Existing.Environments; len(got) != 3 || got[2] != "prod" {
		t.Fatalf("conflict existing = %v", got)
	}

	// Exact-name get; case sensitive.
	got, err := db.GetPromotionRoute("prod-line")
	if err != nil || got == nil || got.Name != "prod-line" {
		t.Fatalf("get = %+v, %v", got, err)
	}
	missing, err := db.GetPromotionRoute("Prod-Line")
	if err != nil || missing != nil {
		t.Fatalf("case-folded get = %+v, %v", missing, err)
	}

	// List ordered by name.
	if _, err := db.CreatePromotionRoute(&PromotionRoute{Name: "alpha", Environments: []string{"dev"}}); err != nil {
		t.Fatalf("create alpha: %v", err)
	}
	routes, err := db.ListPromotionRoutes()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(routes) != 2 || routes[0].Name != "alpha" || routes[1].Name != "prod-line" {
		t.Fatalf("routes = %+v", routes)
	}
	if len(routes[1].Environments) != 3 {
		t.Fatalf("ordered environments = %v", routes[1].Environments)
	}
}

func TestPromotionRouteBatchBinding(t *testing.T) {
	db := openTestStore(t)
	if _, err := db.EnsureEnvironment(&TrackedEnvironment{Environment: "dev"}); err != nil {
		t.Fatalf("ensure env: %v", err)
	}
	first := &PromotionRoute{Name: "first", Environments: []string{"dev"}}
	if _, err := db.CreatePromotionRoute(first); err != nil {
		t.Fatalf("create first: %v", err)
	}
	second := &PromotionRoute{Name: "second", Environments: []string{"dev"}}
	if _, err := db.CreatePromotionRoute(second); err != nil {
		t.Fatalf("create second: %v", err)
	}

	created, err := db.BindReleaseBatchRoute("batch-1", first)
	if err != nil || !created {
		t.Fatalf("first bind = %v, %v", created, err)
	}
	created, err = db.BindReleaseBatchRoute("batch-1", first)
	if err != nil || created {
		t.Fatalf("same bind = %v, %v", created, err)
	}
	_, err = db.BindReleaseBatchRoute("batch-1", second)
	var alreadyBound *ErrPromotionRouteAlreadyBound
	if !errors.As(err, &alreadyBound) || alreadyBound.RouteName != "first" {
		t.Fatalf("rebind err = %v", err)
	}

	name, err := db.GetBoundRouteName("batch-1")
	if err != nil || name != "first" {
		t.Fatalf("bound name = %q, %v", name, err)
	}
	name, err = db.GetBoundRouteName("unbound-batch")
	if err != nil || name != "" {
		t.Fatalf("unbound name = %q, %v", name, err)
	}

	// Binding is batch-scoped: another batch can bind the same or a different
	// route independently; batches are never merged.
	created, err = db.BindReleaseBatchRoute("batch-2", second)
	if err != nil || !created {
		t.Fatalf("batch-2 bind = %v, %v", created, err)
	}
	name, _ = db.GetBoundRouteName("batch-2")
	if name != "second" {
		t.Fatalf("batch-2 name = %q", name)
	}
}
