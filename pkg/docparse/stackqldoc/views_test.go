package stackqldoc_test

import (
	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
	"github.com/stackql-labs/omnisdk/pkg/docparse/stackqldoc"
	"testing"
	"testing/fstest"
)

func TestCatalogExposesViewWithoutRunnableMethod(t *testing.T) {
	fsys := fstest.MapFS{
		"provider.yaml": {Data: []byte("name: p\nproviderServices:\n  s:\n    service: {$ref: 'services/s.yaml'}\n")},
		"services/s.yaml": {Data: []byte(`openapi: 3.0.0
components:
  x-stackQL-resources:
    projected:
      config:
        views:
          select:
            predicate: sqlDialect == "sqlite3"
            ddl: 'SELECT id FROM p.s.items'
            fallback:
              predicate: sqlDialect == "postgres"
              ddl: 'SELECT id FROM p.s.items'
      methods: {}
`)},
	}
	catalog, err := stackqldoc.Open(fsys)
	if err != nil {
		t.Fatal(err)
	}
	views, ok := catalog.(interface {
		View(string, string) (aot.View, bool, error)
	})
	if !ok {
		t.Fatal("view metadata is unavailable")
	}
	view, found, err := views.View("s", "projected")
	if err != nil || !found {
		t.Fatalf("view = %v,%v", found, err)
	}
	if view.DDL() != "SELECT id FROM p.s.items" {
		t.Fatal(view.DDL())
	}
	methods, err := catalog.Methods("s", "projected")
	if err != nil || len(methods) != 1 || methods[0].Name() != "__view__" || methods[0].SQLVerb() != "select" {
		t.Fatalf("view methods=%v, error=%v", methods, err)
	}
	fallback, ok := view.Fallback()
	if !ok || fallback.Predicate() != `sqlDialect == "postgres"` {
		t.Fatalf("fallback = %v", fallback)
	}
}
