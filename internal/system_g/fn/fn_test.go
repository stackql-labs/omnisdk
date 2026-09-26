package fn_test

import (
	"reflect"
	"slices"
	"sort"
	"testing"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/fn"
)

// Every call goes through Invoke: arity and the declared argument kinds are enforced there, so a
// test that called Call directly would be exercising a path no consumer can reach.
func TestSplitPart(t *testing.T) {
	r := builtins(t)
	cases := []struct {
		name string
		args []any
		want any
	}{
		{"first field", []any{"a,b,c", ",", 1}, "a"},
		{"middle field", []any{"a,b,c", ",", 2}, "b"},
		{"from the end", []any{"a,b,c", ",", -1}, "c"},
		{"past the end is empty, not an error", []any{"a,b,c", ",", 9}, ""},
		{"past the start is empty", []any{"a,b,c", ",", -9}, ""},
		{"empty delimiter is one field", []any{"abc", "", 1}, "abc"},
		{"multi-char delimiter", []any{"a::b", "::", 2}, "b"},
		{"float index from JSON", []any{"a,b", ",", float64(2)}, "b"},
		{"null propagates", []any{nil, ",", 1}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := fn.Invoke(r, "split_part", c.args)
			if err != nil {
				t.Fatalf("Call: %v", err)
			}
			if got != c.want {
				t.Fatalf("got %#v, want %#v", got, c.want)
			}
		})
	}
}

func TestSplitPartRejectsZeroAndNonIntegral(t *testing.T) {
	r := builtins(t)
	for _, args := range [][]any{
		{"a,b", ",", 0},
		{"a,b", ",", 1.5},
		{"a,b", ",", "x"},
	} {
		if _, err := fn.Invoke(r, "split_part", args); err == nil {
			t.Fatalf("Invoke(%#v): want error, got none", args)
		}
	}
}

// An argument is parsed through the kind the signature DECLARES, so a value the kind cannot read is
// rejected before the function runs — the declaration is enforcement, not documentation.
func TestDeclaredKindsAreEnforcedBeforeCall(t *testing.T) {
	r := builtins(t)
	if _, err := fn.Invoke(r, "split_part", []any{"a,b", ",", "x"}); err == nil {
		t.Fatal("non-integer where int is declared: want error, got none")
	}
	// An integer written as text still parses as the declared int kind.
	got, err := fn.Invoke(r, "split_part", []any{"a,b", ",", "2"})
	if err != nil || got != "b" {
		t.Fatalf("Invoke = %#v, %v", got, err)
	}
}

// One name, several signatures: the optional null_string is a second signature, resolved by arity.
func TestOverloadResolvesByArity(t *testing.T) {
	r := builtins(t)
	two, err := fn.Invoke(r, "string_to_table", []any{"a,NUL,b", ","})
	if err != nil {
		t.Fatalf("two-arg: %v", err)
	}
	if !reflect.DeepEqual(two, []any{
		map[string]any{"value": "a"},
		map[string]any{"value": "NUL"},
		map[string]any{"value": "b"},
	}) {
		t.Fatalf("two-arg = %#v", two)
	}
	three, err := fn.Invoke(r, "string_to_table", []any{"a,NUL,b", ",", "NUL"})
	if err != nil {
		t.Fatalf("three-arg: %v", err)
	}
	if !reflect.DeepEqual(three, []any{
		map[string]any{"value": "a"},
		map[string]any{"value": nil},
		map[string]any{"value": "b"},
	}) {
		t.Fatalf("three-arg = %#v", three)
	}
}

func builtins(t *testing.T) facade.FnRegistry {
	t.Helper()
	r, err := fn.Builtins("value")
	if err != nil {
		t.Fatalf("Builtins: %v", err)
	}
	return r
}

func TestStringToTableEmitsRowsUnderTheConstructedColumn(t *testing.T) {
	f := fn.NewStringToTable("value")
	r, err := fn.NewRegistry(f)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	got, err := fn.Invoke(r, "string_to_table", []any{"a,b,c", ","})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	want := []any{
		map[string]any{"value": "a"},
		map[string]any{"value": "b"},
		map[string]any{"value": "c"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
	cols := f.Signatures()[0].Columns()
	if len(cols) != 1 || cols[0].Name() != "value" || cols[0].Kind().Name() != "string" {
		t.Fatalf("Columns() = %#v", cols)
	}
}

// The column name is the caller's, not a constant: there is no planner above this to alias it.
func TestStringToTableColumnIsTheCallersChoice(t *testing.T) {
	f := fn.NewStringToTable("part")
	r, err := fn.NewRegistry(f)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	got, err := fn.Invoke(r, "string_to_table", []any{"a", ","})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !reflect.DeepEqual(got, []any{map[string]any{"part": "a"}}) {
		t.Fatalf("got %#v", got)
	}
}

func TestRegistryRejectsDuplicates(t *testing.T) {
	if _, err := fn.NewRegistry(fn.NewSplitPart(), fn.NewSplitPart()); err == nil {
		t.Fatal("want duplicate error, got none")
	}
}

func TestInvokeEnforcesArityAndReportsUnknown(t *testing.T) {
	r := builtins(t)
	if _, err := fn.Invoke(r, "split_part", []any{"a,b", ","}); err == nil {
		t.Fatal("too few arguments: want error, got none")
	}
	if _, err := fn.Invoke(r, "split_part", []any{"a,b", ",", 1, 2}); err == nil {
		t.Fatal("too many arguments: want error, got none")
	}
	if _, err := fn.Invoke(r, "no_such_fn", nil); err == nil {
		t.Fatal("unknown function: want error, got none")
	}
	got, err := fn.Invoke(r, "split_part", []any{"a,b", ",", 2})
	if err != nil || got != "b" {
		t.Fatalf("Invoke = %#v, %v", got, err)
	}
}

func TestBuiltinsAreListedInNameOrder(t *testing.T) {
	r := builtins(t)
	var names []string
	for _, f := range r.Fns() {
		names = append(names, f.Name())
	}
	if !sort.StringsAreSorted(names) {
		t.Fatalf("Fns() = %v, want name order", names)
	}
	for _, want := range []string{"split_part", "string_to_table", "json_extract", "json_each"} {
		if !slices.Contains(names, want) {
			t.Errorf("Fns() lacks %s", want)
		}
	}
}

// A catalogue function is called with the row's values as they are, and a table one yields rows.
func TestCatalogueFunctionsRunThroughInvoke(t *testing.T) {
	r := builtins(t)
	got, err := fn.Invoke(r, "json_extract", []any{map[string]any{"a": []any{1.0, 2.0}}, "$.a[1]"})
	if err != nil || got != 2.0 {
		t.Fatalf("json_extract = %#v, %v", got, err)
	}
	rows, err := fn.RowProducing(r, "json_each")
	if err != nil || !rows {
		t.Fatalf("json_each row-producing = %v, %v", rows, err)
	}
	if _, err := fn.Invoke(r, "lower", []any{"a", "b"}); err == nil {
		t.Error("lower accepted two arguments")
	}
}
