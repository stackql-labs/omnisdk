package omnisdk

import (
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
)

type debugBindingFixture struct{}

func (debugBindingFixture) Target() string    { return "create.label" }
func (debugBindingFixture) Attribute() string { return "debug" }
func (debugBindingFixture) CreateParams() map[string]string {
	return map[string]string{"create.key": "debug-label"}
}

type debugCatalogFixture struct{ aot.Catalog }

func (debugCatalogFixture) DebugCorrelationBindings(string) ([]aot.DebugCorrelationBinding, error) {
	return []aot.DebugCorrelationBinding{debugBindingFixture{}}, nil
}

func TestDebugBindingIsExplicitAndDoesNotChangeCallerInput(t *testing.T) {
	for _, target := range []string{"", "create.label"} {
		t.Run(target, func(t *testing.T) {
			params := map[string]string{"scope": "one"}
			original := NewResource("resource", "provider", "service.resource", []byte(`{"name":"one"}`), params, nil, "", "", "id", "id", target)
			bound, err := bindDebugCorrelation(original, debugCatalogFixture{}, "address", "deployment")
			if err != nil {
				t.Fatal(err)
			}
			state, err := decodeObject(bound.Desired())
			if err != nil {
				t.Fatal(err)
			}
			if target == "" {
				if state["debug"] != nil || bound.Params()["create.label"] != "" || bound.Params()["create.key"] != "" {
					t.Fatalf("disabled binding: %v %v", state, bound.Params())
				}
			} else if state["debug"] != "deployment/resource" || bound.Params()["create.label"] != "deployment/resource" || bound.Params()["create.key"] != "debug-label" {
				t.Fatalf("enabled binding: %v %v", state, bound.Params())
			}
			if len(params) != 1 || string(original.Desired()) != `{"name":"one"}` {
				t.Fatal("caller input changed")
			}
		})
	}
	bad := NewResource("resource", "provider", "service.resource", []byte(`{}`), nil, nil, "", "", "id", "id", "unsupported")
	if _, err := bindDebugCorrelation(bad, debugCatalogFixture{}, "address", "deployment"); err == nil {
		t.Fatal("unsupported target accepted")
	}
}
