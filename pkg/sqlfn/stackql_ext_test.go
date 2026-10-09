package sqlfn_test

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/sqlfn"
)

// stackql's own functions are pinned by any-sdk's golden vectors (testdata/anysdk, copied from
// any-sdk v0.6.0-alpha01 public/sqlfuncs/testdata), run through the SQLite catalogue.
func TestStackqlExtensionGoldenVectors(t *testing.T) {
	cat := sqlfn.BuiltinsFor(sqlfn.SQLite)
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
