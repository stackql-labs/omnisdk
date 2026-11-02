package sqlfn_test

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// stackql's own functions are pinned by any-sdk's golden vectors (testdata/anysdk, copied from
// any-sdk v0.6.0-alpha01 public/sqlfuncs/testdata), run through the SQLite catalogue.
func TestStackqlExtensionGoldenVectors(t *testing.T) {
	cat := sqliteCatalog(t)
	norm := func(v any) any {
		if f, ok := v.(float64); ok && math.Trunc(f) == f && !math.IsInf(f, 0) {
			return int64(f)
		}
		return v
	}
	for _, name := range []string{"split_part", "regexp_like", "regexp", "regexp_substr", "regexp_replace",
		"json_equal", "aws_policy_equal"} {
		raw, err := os.ReadFile(filepath.Join("testdata", "anysdk", name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Function string `json:"function"`
			Vectors  []struct {
				ID     string `json:"id"`
				Args   []any  `json:"args"`
				Result any    `json:"result"`
				Error  bool   `json:"error"`
			} `json:"vectors"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		fn, ok := cat.Get(name)
		if !ok {
			t.Fatalf("%s missing", name)
		}
		if len(doc.Vectors) == 0 {
			t.Fatalf("%s: no vectors", name)
		}
		for _, v := range doc.Vectors {
			for i := range v.Args {
				v.Args[i] = norm(v.Args[i])
			}
			got, err := fn.Call(v.Args)
			switch {
			case v.Error && err == nil:
				t.Errorf("%s %s: want an error, got %v", name, v.ID, got)
			case !v.Error && err != nil:
				t.Errorf("%s %s: unexpected error %v", name, v.ID, err)
			case !v.Error && got != norm(v.Result):
				t.Errorf("%s %s(%v) = %#v, want %#v", name, v.ID, v.Args, got, v.Result)
			}
		}
	}
}

func TestStackqlStringConstructor(t *testing.T) {
	cat := sqliteCatalog(t)
	function, ok := cat.Get("string")
	if !ok {
		t.Fatal("string constructor missing")
	}
	for _, value := range []any{`{"BucketName":"my-bucket"}`, "", nil, int64(10)} {
		got, err := function.Call([]any{value})
		if err != nil {
			t.Fatal(err)
		}
		want := value
		if _, number := value.(int64); number {
			want = "10"
		}
		if got != want {
			t.Fatalf("string(%v) = %v, want %v", value, got, want)
		}
	}
}

func TestStackqlMD5Extension(t *testing.T) {
	fn, ok := sqliteCatalog(t).Get("md5")
	if !ok {
		t.Fatal("md5 missing")
	}
	for _, tc := range []struct {
		input any
		want  any
	}{
		{"42-east", "69dbfdc8fb30e0e779dfbe961105a516"},
		{nil, nil},
	} {
		got, err := fn.Call([]any{tc.input})
		if err != nil || got != tc.want {
			t.Fatalf("md5(%v) = %v, %v; want %v", tc.input, got, err, tc.want)
		}
	}
}
