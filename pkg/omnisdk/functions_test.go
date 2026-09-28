package omnisdk_test

import (
	"reflect"
	"slices"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
)

func TestFunctionCatalogPublishesSignatures(t *testing.T) {
	c, err := omnisdk.Functions("value")
	if err != nil {
		t.Fatalf("Functions: %v", err)
	}
	var names []string
	for _, f := range c.Functions() {
		names = append(names, f.Name())
	}
	// Every function the stackql test suites call row-by-row is listed.
	for _, want := range []string{"split_part", "string_to_table", "json_extract", "json_extract_path_text",
		"json_each", "json_array_elements_text", "lower", "substr", "instr", "regexp_replace", "date",
		"datetime", "julianday", "coalesce", "typeof", "aws_policy_equal", "json_equal"} {
		if !slices.Contains(names, want) {
			t.Errorf("Functions() lacks %s", want)
		}
	}

	sp, ok := c.GetFunction("split_part")
	if !ok {
		t.Fatal("split_part absent")
	}
	want := []omnisdk.Signature{{
		Args: []string{"string", "string", "int"}, Returns: "string",
	}}
	if got := sp.Signatures(); !reflect.DeepEqual(got, want) {
		t.Fatalf("split_part signatures = %#v", got)
	}

	// One name, two signatures: the optional null_string is declared, not hidden behind a flag.
	stt, _ := c.GetFunction("string_to_table")
	sigs := stt.Signatures()
	if len(sigs) != 2 {
		t.Fatalf("string_to_table signatures = %#v", sigs)
	}
	if !reflect.DeepEqual(sigs[0].Columns, []omnisdk.Column{{Name: "value", Kind: "string"}}) {
		t.Fatalf("columns = %#v", sigs[0].Columns)
	}
	if sigs[0].Returns != "object" {
		t.Fatalf("a row-producing signature returns object, got %q", sigs[0].Returns)
	}
}

func TestFunctionCatalogCalls(t *testing.T) {
	c, err := omnisdk.Functions("value")
	if err != nil {
		t.Fatalf("Functions: %v", err)
	}
	got, err := c.Call("split_part", []any{"a,b,c", ",", 2})
	if err != nil || got != "b" {
		t.Fatalf("split_part = %#v, %v", got, err)
	}
	rows, err := c.Call("string_to_table", []any{"a,b", ","})
	if err != nil {
		t.Fatalf("string_to_table: %v", err)
	}
	want := []any{map[string]any{"value": "a"}, map[string]any{"value": "b"}}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("string_to_table = %#v", rows)
	}
	if _, err := c.Call("split_part", []any{"a,b"}); err == nil {
		t.Fatal("arity is enforced by the catalog: want error, got none")
	}
}

// The emitted column name is the caller's input, not a constant baked into the module.
func TestRowColumnIsCallerSupplied(t *testing.T) {
	c, err := omnisdk.Functions("part")
	if err != nil {
		t.Fatalf("Functions: %v", err)
	}
	rows, err := c.Call("string_to_table", []any{"a", ","})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !reflect.DeepEqual(rows, []any{map[string]any{"part": "a"}}) {
		t.Fatalf("got %#v", rows)
	}
}
