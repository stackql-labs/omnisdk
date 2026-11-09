package stackqldoc

import (
	"gopkg.in/yaml.v3"
	"testing"
)

func configFrom(t *testing.T, text string) config {
	t.Helper()
	var c config
	if err := yaml.Unmarshal([]byte(text), &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestConfigDeepestPathWinsForEveryMember(t *testing.T) {
	original := configDefaults
	defer func() { configDefaults = original }()
	configDefaults = []configDefault{
		{Path: []string{"p"}, Config: map[string]any{"future": map[string]any{"a": "provider", "b": "inherited"}}},
		{Path: []string{"p", "s", "r"}, Config: map[string]any{"future": map[string]any{"a": "resource"}}},
		{Path: []string{"p", "s", "renamed"}, Config: map[string]any{"future": map[string]any{"a": "wrong"}}},
	}
	provider := configFrom(t, "future: {a: provider-document}")
	resource := configFrom(t, "future: {c: false}")
	method := configFrom(t, "future: {a: method, b: null}")
	resolved, err := resolveConfig([]string{"p", "s", "r", "m"}, provider, config{}, resource, method)
	if err != nil {
		t.Fatal(err)
	}
	future := resolved.raw["future"].(map[string]any)
	if future["a"] != "method" || future["b"] != nil || future["c"] != false {
		t.Fatalf("resolved=%v", future)
	}
	resolved, err = resolveConfig([]string{"p", "s", "r"}, provider, config{}, resource)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.raw["future"].(map[string]any)["a"] != "resource" {
		t.Fatal("broad document overrode finer default")
	}
	resource = configFrom(t, "future: {a: resource-document}")
	resolved, err = resolveConfig([]string{"p", "s", "r"}, provider, config{}, resource)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.raw["future"].(map[string]any)["a"] != "resource-document" {
		t.Fatal("default overrode document at same depth")
	}
}

func TestGoogleDefaultsAndDisable(t *testing.T) {
	for _, provider := range []string{"google", "stackql_unstable_google"} {
		c, err := resolveConfig([]string{provider, "compute", "networks", "insert"})
		if err != nil || c.Lifecycle == nil || c.Lifecycle.StateDerivation != "self_read_returns_data" {
			t.Fatalf("config=%v err=%v", c, err)
		}
		c, err = resolveConfig([]string{provider, "compute", "networks", "insert"}, config{}, config{}, configFrom(t, "lifecycle: null"))
		if err != nil || c.Lifecycle != nil {
			t.Fatalf("disable config=%v err=%v", c, err)
		}
	}
	c, err := resolveConfig([]string{"other", "compute", "networks", "insert"})
	if err != nil || c.Lifecycle != nil {
		t.Fatalf("unmatched config=%v err=%v", c, err)
	}
}
