package resourcebindings_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stackql-labs/omnisdk/internal/resourcebindings"
)

func ref(source, attribute string) map[string]any {
	return map[string]any{"from": source, "attribute": attribute}
}
func TestResolveStructuredReferences(t *testing.T) {
	sources := map[string]map[string]any{
		"self":  {"key": "vpc", "desired": map[string]any{"name": ref("input", "name")}},
		"input": {"name": "chosen", "enabled": false, "count": json.Number("9007199254740993"), "null": nil, "object": map[string]any{"from": "literal", "attribute": "ordinary-data"}},
		"env":   {"REGION": "ap-southeast-2", "EMPTY": ""},
	}
	value := map[string]any{"key": ref("self", "key"), "nested": []any{ref("self", "desired.name"), ref("input", "count"), ref("input", "enabled"), ref("input", "null")}, "region": ref("env", "REGION"), "empty": ref("env", "EMPTY"), "object": ref("input", "object"), "literal": "${self}.key"}
	out, err := resourcebindings.New().Resolve(value, sources)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(out)
	want := `{"empty":"","key":"vpc","literal":"${self}.key","nested":["chosen",9007199254740993,false,null],"object":{"attribute":"ordinary-data","from":"literal"},"region":"ap-southeast-2"}`
	if string(got) != want {
		t.Fatalf("got %s", got)
	}
	out.(map[string]any)["object"].(map[string]any)["from"] = "modified"
	if sources["input"]["object"].(map[string]any)["from"] != "literal" {
		t.Fatal("source mutated")
	}
}
func TestInvalidBindingsFailWithoutFallback(t *testing.T) {
	for _, tc := range []struct {
		name    string
		value   any
		sources map[string]map[string]any
		error   string
	}{
		{"missing-env", ref("env", "NAME"), map[string]map[string]any{"input": {"NAME": "must-not-fallback"}}, "unresolved env"},
		{"missing-input", ref("input", "NAME"), nil, "unresolved input"},
		{"unknown-source", ref("guess", "key"), nil, "unknown binding source"},
		{"bad-attribute", map[string]any{"from": "self", "attribute": 3}, nil, "attribute"},
		{"extra-option", map[string]any{"from": "input", "attribute": "key", "default": "hidden"}, nil, "only from and attribute"},
		{"cycle", ref("self", "one"), map[string]map[string]any{"self": {"one": ref("self", "two"), "two": ref("self", "one")}}, "cyclic self"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resourcebindings.New().Resolve(tc.value, tc.sources)
			if err == nil || !strings.Contains(err.Error(), tc.error) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}
func TestEnvironmentNamesAreOnlyExplicitReferences(t *testing.T) {
	names, err := resourcebindings.New().References(map[string]any{"one": ref("env", "Z"), "two": []any{ref("env", "A"), ref("env", "Z")}, "literal": "$OTHER"}, "env")
	if err != nil || strings.Join(names, ",") != "A,Z" {
		t.Fatalf("names=%v error=%v", names, err)
	}
}
