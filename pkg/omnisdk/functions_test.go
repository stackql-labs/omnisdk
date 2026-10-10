package omnisdk_test

import (
	"reflect"
	"slices"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
	"github.com/stackql-labs/omnisdk/pkg/sqlfn"
)

func functionNames(t *testing.T, d sqlfn.Dialect) (omnisdk.FunctionCatalog, []string) {
	t.Helper()
	c, err := omnisdk.Functions(d)
	if err != nil {
		t.Fatalf("Functions(%q): %v", d, err)
	}
	var names []string
	for _, f := range c.Functions() {
		names = append(names, f.Name())
	}
	return c, names
}

func TestFunctionCatalogPublishesSignatures(t *testing.T) {
	_, sqlite := functionNames(t, sqlfn.SQLite)
	for _, want := range []string{"json_extract", "json_each", "lower", "substr", "instr", "date", "datetime",
		"julianday", "coalesce", "typeof", "printf", "aws_policy_equal", "json_equal", "||", "cast"} {
		if !slices.Contains(sqlite, want) {
			t.Errorf("SQLite Functions() lacks %s", want)
		}
	}
	pg, postgres := functionNames(t, sqlfn.Postgres)
	for _, want := range []string{"split_part", "string_to_table", "json_extract_path_text", "json_array_elements_text",
		"jsonb_set", "generate_series", "format", "||", "cast"} {
		if !slices.Contains(postgres, want) {
			t.Errorf("Postgres Functions() lacks %s", want)
		}
	}
	if slices.Contains(postgres, "json_extract") || slices.Contains(sqlite, "string_to_table") {
		t.Error("a dialect lists another's function")
	}

	// A catalogue function takes row values as they are, one signature per arity.
	sp, _ := pg.GetFunction("split_part")
	if got, want := sp.Signatures(), []omnisdk.Signature{{Args: []string{"unknown", "unknown", "unknown"}, Returns: "unknown"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("split_part signatures = %#v", got)
	}
	stt, _ := pg.GetFunction("string_to_table")
	sigs := stt.Signatures()
	if len(sigs) != 2 || sigs[0].Returns != "object" {
		t.Fatalf("string_to_table signatures = %#v", sigs)
	}
	if !reflect.DeepEqual(sigs[0].Columns, []omnisdk.Column{{Name: "string_to_table", Kind: "unknown"}}) {
		t.Fatalf("columns = %#v", sigs[0].Columns)
	}
}

// json_each's json and root are published, marked hidden.
func TestFunctionCatalogPublishesHiddenColumns(t *testing.T) {
	c, _ := functionNames(t, sqlfn.SQLite)
	each, _ := c.GetFunction("json_each")
	var hidden []string
	for _, col := range each.Signatures()[0].Columns {
		if col.Hidden {
			hidden = append(hidden, col.Name)
		}
	}
	if !reflect.DeepEqual(hidden, []string{"json", "root"}) {
		t.Fatalf("hidden = %v", hidden)
	}
}

func TestFunctionCatalogCalls(t *testing.T) {
	sqlite, _ := functionNames(t, sqlfn.SQLite)
	if got, err := sqlite.Call("substr", []any{"abcdef", 2, 3}); err != nil || got != "bcd" {
		t.Fatalf("substr = %#v, %v", got, err)
	}
	pg, _ := functionNames(t, sqlfn.Postgres)
	rows, err := pg.Call("string_to_table", []any{"a,b", ","})
	if err != nil {
		t.Fatalf("string_to_table: %v", err)
	}
	want := []any{map[string]any{"string_to_table": "a"}, map[string]any{"string_to_table": "b"}}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("string_to_table = %#v", rows)
	}
	// A call's arguments are values: a Go int is a bigint, and Postgres has no split_part(text,
	// text, bigint).
	if _, err := pg.Call("split_part", []any{"a,b,c", ",", 2}); err == nil {
		t.Fatal("split_part over a bigint: want error, got none")
	}
	if _, err := pg.Call("split_part", []any{"a,b"}); err == nil {
		t.Fatal("arity is enforced by the catalog: want error, got none")
	}
}
