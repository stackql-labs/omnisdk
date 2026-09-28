package sqlfn_test

import (
	"reflect"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/sqlfn"
)

func call(t *testing.T, name string, args ...any) any {
	t.Helper()
	f, ok := sqlfn.Builtins().Get(name)
	if !ok {
		t.Fatalf("no function %q", name)
	}
	v, err := f.Call(args)
	if err != nil {
		t.Fatalf("%s(%v): %v", name, args, err)
	}
	return v
}

func TestScalars(t *testing.T) {
	doc := `{"a":{"b":[10,{"c":"x"}],"t":true},"k y":"q"}`
	cases := []struct {
		name string
		args []any
		want any
	}{
		{"json_extract", []any{doc, "$.a.b[0]"}, float64(10)},
		{"json_extract", []any{doc, "$.a.b[1].c"}, "x"},
		{"json_extract", []any{doc, "$.a.b[#-1].c"}, "x"},
		{"json_extract", []any{doc, `$."k y"`}, "q"},
		{"json_extract", []any{doc, "$.a.t"}, int64(1)},
		{"json_extract", []any{doc, "$.a.b"}, `[10,{"c":"x"}]`},
		{"json_extract", []any{doc, "$.missing"}, nil},
		{"json_extract", []any{doc, "$.a.t", "$.missing"}, `[true,null]`},
		{"json_extract_path_text", []any{doc, "a", "b", "1", "c"}, "x"},
		{"json_extract_path_text", []any{doc, "a", "t"}, "true"},
		{"json_array_length", []any{`[1,2,3]`}, int64(3)},
		{"json_array_length", []any{doc, "$.a.b"}, int64(2)},
		{"json_type", []any{doc, "$.a"}, "object"},
		{"json_object", []any{"a", 1, "b", `{"c":2}`}, `{"a":1,"b":{"c":2}}`},
		{"json_equal", []any{`{"a":1,"b":[1,2]}`, `{"b":[1,2],"a":1}`}, true},
		{"aws_policy_equal", []any{
			`{"Version":"2012-10-17","Statement":{"Effect":"Allow","Action":"s3:GetObject","Resource":"*"}}`,
			`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:GetObject"],"Resource":["*"]}]}`,
		}, true},
		{"lower", []any{"AbC"}, "abc"},
		{"substr", []any{"abcdef", 2, 3}, "bcd"},
		{"substr", []any{"abcdef", -2}, "ef"},
		{"instr", []any{"abcdef", "cd"}, int64(3)},
		{"split_part_absent", nil, nil},
		{"regexp_replace", []any{"a-b-c", "-", "_"}, "a_b-c"},
		{"regexp_replace", []any{"a-b-c", "-", "_", "g"}, "a_b_c"},
		{"regexp_replace", []any{"ab12", `([a-z]+)(\d+)`, `\2\1`}, "12ab"},
		{"regexp_substr", []any{"id=42;", `\d+`}, "42"},
		{"regexp_like", []any{"Prod-1", "^prod", "i"}, true},
		{"coalesce", []any{nil, nil, "z"}, "z"},
		{"typeof", []any{float64(3)}, "integer"},
		{"typeof", []any{"x"}, "text"},
		{"abs", []any{-2.5}, 2.5},
		{"floor", []any{"2.7"}, float64(2)},
		{"date", []any{"2026-09-26T10:11:12Z"}, "2026-09-26"},
		{"datetime", []any{"2026-09-26 10:11:12", "+1 day"}, "2026-09-27 10:11:12"},
		{"datetime", []any{"1700000000", "unixepoch"}, "2023-11-14 22:13:20"},
		{"date", []any{"2026-09-26", "start of month"}, "2026-09-01"},
		{"lower", []any{nil}, nil},
	}
	for _, c := range cases {
		if c.name == "split_part_absent" {
			if _, ok := sqlfn.Builtins().Get("split_part"); ok {
				t.Error("split_part is the engine's, not duplicated here")
			}
			continue
		}
		if got := call(t, c.name, c.args...); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s(%v) = %#v, want %#v", c.name, c.args, got, c.want)
		}
	}
}

func TestTables(t *testing.T) {
	got := call(t, "json_each", `{"b":2,"a":[1]}`)
	want := []map[string]any{
		{"key": "a", "value": "[1]", "type": "array"},
		{"key": "b", "value": float64(2), "type": "integer"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("json_each = %#v", got)
	}
	if got := call(t, "json_array_elements_text", `["x",{"y":1}]`); !reflect.DeepEqual(got,
		[]map[string]any{{"value": "x"}, {"value": `{"y":1}`}}) {
		t.Errorf("json_array_elements_text = %#v", got)
	}
	if got := call(t, "generate_subscripts", `["x","y"]`, 1); !reflect.DeepEqual(got,
		[]map[string]any{{"value": int64(1)}, {"value": int64(2)}}) {
		t.Errorf("generate_subscripts = %#v", got)
	}
}

func TestCatalogRefusesDuplicates(t *testing.T) {
	f, _ := sqlfn.Builtins().Get("lower")
	if _, err := sqlfn.With(sqlfn.Builtins(), f); err == nil {
		t.Error("accepted a second lower")
	}
}
