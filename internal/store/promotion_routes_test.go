package store

import (
	"reflect"
	"testing"
)

func TestPromotionRouteStoreRoundTrip(t *testing.T) {
	db := openTestStore(t)
	route := &PromotionRoute{Name: "prod-line", Environments: []string{"dev", "test", "prod"}}
	if err := db.InsertPromotionRoute(route); err != nil {
		t.Fatalf("insert route: %v", err)
	}
	if err := db.InsertPromotionRoute(&PromotionRoute{Name: "alpha-line", Environments: []string{"dev"}}); err != nil {
		t.Fatalf("insert second route: %v", err)
	}

	got, err := db.GetPromotionRoute("prod-line")
	if err != nil || got == nil {
		t.Fatalf("get route = %+v, %v", got, err)
	}
	if !reflect.DeepEqual(got.Environments, []string{"dev", "test", "prod"}) {
		t.Fatalf("environments = %v, want stored order", got.Environments)
	}
	missing, err := db.GetPromotionRoute("Prod-Line")
	if err != nil || missing != nil {
		t.Fatalf("case-mismatched lookup = %+v, %v; want nil", missing, err)
	}

	routes, err := db.ListPromotionRoutes()
	if err != nil || len(routes) != 2 {
		t.Fatalf("list = %+v, %v", routes, err)
	}
	if routes[0].Name != "alpha-line" || routes[1].Name != "prod-line" {
		t.Fatalf("list order = %v, %v; want name ascending", routes[0].Name, routes[1].Name)
	}
}

func TestBatchPromotionRouteBinding(t *testing.T) {
	db := openTestStore(t)
	if _, found, err := db.BatchPromotionRoute("batch-1"); err != nil || found {
		t.Fatalf("unbound lookup = %v, %v; want false", found, err)
	}
	if err := db.BindBatchPromotionRoute("batch-1", "prod-line"); err != nil {
		t.Fatalf("bind: %v", err)
	}
	name, found, err := db.BatchPromotionRoute("batch-1")
	if err != nil || !found || name != "prod-line" {
		t.Fatalf("bound lookup = %q, %v, %v", name, found, err)
	}
	if _, found, err := db.BatchPromotionRoute("batch-2"); err != nil || found {
		t.Fatalf("other batch lookup = %v, %v; want false", found, err)
	}
}
