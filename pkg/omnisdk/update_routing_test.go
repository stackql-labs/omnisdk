package omnisdk_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
)

func updateWidgetBundle(t *testing.T, routes string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"provider.yaml", "services/things.yaml"} {
		b, err := os.ReadFile(filepath.Join(authRegistry, "widgets/v00.00.00000", name))
		if err != nil {
			t.Fatal(err)
		}
		if name != "provider.yaml" {
			b = []byte(strings.Replace(string(b), "    widgets:\n      id:", "    widgets:\n"+routes+"      id:", 1))
		}
		p := filepath.Join(dir, "widgets", "v00.00.00000", name)
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const colorUpdateRoutes = `      updates:
        - priority: 100
          requires: [color]
          permits: [color]
          target: {$ref: '#/components/x-stackQL-resources/widgets/methods/update'}
        - priority: 10
          permits: [color, size]
          target: '#/components/x-stackQL-resources/widgets/methods/update'
`

func TestPublicUpdateCandidatesDoNotExecute(t *testing.T) {
	dir := updateWidgetBundle(t, colorUpdateRoutes)
	for _, tc := range []struct {
		changes    []string
		priorities []int
	}{{[]string{"color"}, []int{100, 10}}, {[]string{"size"}, []int{10}}, {nil, nil}, {[]string{"unhandled"}, nil}} {
		candidates, err := omnisdk.UpdateCandidates(context.Background(), dir, map[string][]string{widgets: tc.changes}, omnisdk.Args{})
		if err != nil {
			t.Fatal(err)
		}
		var got []int
		for _, c := range candidates {
			got = append(got, c.Priority())
			if c.TargetResource() != widgets || c.TargetMethod() != "update" || c.Resource() != widgets || !strings.Contains(c.Source(), "services/things.yaml") || c.Location() == "" {
				t.Fatalf("candidate=%v", c)
			}
		}
		if !slices.Equal(got, tc.priorities) {
			t.Fatalf("priorities=%v want=%v", got, tc.priorities)
		}
	}
}
func TestDesiredStateUsesDeclaredUpdateRoutes(t *testing.T) {
	onlyColor := strings.Split(colorUpdateRoutes, "        - priority: 10\n")[0]
	dir := updateWidgetBundle(t, onlyColor)
	state := t.TempDir()
	store := &widgetStore{items: map[string]map[string]any{}}
	srv := store.serve(t)
	defer srv.Close()
	run := func(id, color string) ([]omnisdk.Row, error) {
		resource := omnisdk.NewResource("widget", "widgets", "things.widgets", []byte(`{"size":"S","color":"`+color+`"}`), nil, nil, "", "", "id", "id", "")
		p, err := omnisdk.Prepare(dir, omnisdk.DesiredState([]omnisdk.ManagedResource{resource}), omnisdk.NewLedger("routing", state, id), omnisdk.Args{Endpoint: srv.URL})
		if err != nil {
			return nil, err
		}
		rows, err := p.Open(context.Background())
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []omnisdk.Row
		for rows.Next() {
			out = append(out, rows.Row())
		}
		return out, rows.Err()
	}
	check := func(id, color string) {
		t.Helper()
		rows, err := run(id, color)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if r["error"] != nil && r["error"] != "" {
				t.Fatalf("failed run %s: %v", id, rows)
			}
		}
	}
	check("create", "red")
	if got := store.took(); !slices.Equal(got, []string{"create"}) {
		t.Fatalf("initial calls=%v", got)
	}
	check("unchanged", "red")
	if got := store.took(); !slices.Equal(got, []string{"find"}) {
		t.Fatalf("unchanged calls=%v", got)
	}
	// Reusing the create journal must fail before reading or changing the provider,
	// even when the new intent would otherwise fail route selection and unwind.
	store.mu.Lock()
	store.items["w-1"]["size"] = "L"
	store.mu.Unlock()
	if rows, err := run("create", "blue"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("reused create journal accepted: rows=%v err=%v", rows, err)
	}
	if got := store.took(); len(got) != 0 {
		t.Fatalf("reused run reached provider: %v", got)
	}
	if len(store.items) != 1 {
		t.Fatalf("reused run destroyed existing resource: %v", store.items)
	}
	store.mu.Lock()
	store.items["w-1"]["size"] = "S"
	store.mu.Unlock()
	check("update", "blue")
	if got := store.took(); !slices.Equal(got, []string{"find", "update"}) {
		t.Fatalf("update calls=%v", got)
	}
	store.mu.Lock()
	store.items["w-1"]["size"] = "L"
	store.mu.Unlock()
	rows, err := run("blocked", "blue")
	encoded, _ := json.Marshal(rows)
	if err == nil && !strings.Contains(string(encoded), "no declared update route") {
		t.Fatalf("unhandled drift accepted: rows=%s err=%v", encoded, err)
	}
	if got := store.took(); !slices.Equal(got, []string{"find"}) {
		t.Fatalf("blocked update made effects: %v", got)
	}
}
func TestManagedGraphCollectsAllUndeclaredMutations(t *testing.T) {
	g, err := omnisdk.NewGraph([]omnisdk.Node{omnisdk.NewMutationNode("new_network", "demo.networks", "insert", nil, nil, nil), omnisdk.NewMutationNode("new_lb", "demo.lbs", "insert", nil, nil, nil)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = omnisdk.ConvergeGraph("unused", "deployment", t.TempDir(), "run", g, nil, omnisdk.Args{})
	if err == nil || !strings.Contains(err.Error(), "new_network") || !strings.Contains(err.Error(), "new_lb") {
		t.Fatalf("blockers=%v", err)
	}
}
