package encode_test

import (
	"bytes"
	"encoding/json"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/stackql-labs/omnisdk/pkg/encode"
)

func TestText(t *testing.T) {
	type unexported struct {
		B float64 `json:"b"`
		A string  `json:"a"`
	}
	for name, c := range map[string]struct {
		in   any
		want string
		ok   bool
	}{
		"nil":                  {nil, "", false},
		"string":               {"hello", "hello", true},
		"html stays":           {"<script>&</script>", "<script>&</script>", true},
		"bool":                 {true, "true", true},
		"float positional":     {float64(12000000), "12000000", true},
		"float fraction":       {1.5, "1.5", true},
		"float tiny":           {1e-7, "0.0000001", true},
		"float32":              {float32(2.5), "2.5", true},
		"NaN":                  {math.NaN(), "", false},
		"Inf":                  {math.Inf(1), "", false},
		"json.Number beyond":   {json.Number("9007199254740993"), "9007199254740993", true},
		"int":                  {int64(-42), "-42", true},
		"uint":                 {uint64(18446744073709551615), "18446744073709551615", true},
		"time":                 {time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("x", 3600)), "2026-01-02T02:04:05Z", true},
		"map sorted":           {map[string]any{"team": "platform", "env": "dev"}, `{"env":"dev","team":"platform"}`, true},
		"slice":                {[]any{"a", "b"}, `["a","b"]`, true},
		"slice of maps":        {[]any{map[string]any{"z": 1.0, "a": json.Number("2")}}, `[{"a":2,"z":1}]`, true},
		"nested float no exp":  {map[string]any{"n": 1.2e7}, `{"n":12000000}`, true},
		"nested NaN is null":   {map[string]any{"n": math.NaN()}, `{"n":null}`, true},
		"html in nested":       {map[string]any{"s": "<b>"}, `{"s":"<b>"}`, true},
		"anything else, rules": {unexported{B: 1.2e7, A: "<x>"}, `{"a":"<x>","b":12000000}`, true},
	} {
		got, ok := encode.Text(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("%s: Text = %q, %v; want %q, %v", name, got, ok, c.want, c.ok)
		}
	}
}

func TestJSON(t *testing.T) {
	for name, c := range map[string]struct {
		in   any
		want string
	}{
		"nil":    {nil, `null`},
		"string": {"<script>", `"<script>"`},
		"number": {json.Number("9007199254740993"), `9007199254740993`},
		"float":  {float64(12000000), `12000000`},
		"object": {map[string]any{"labels": map[string]any{"team": "p", "env": "d"}}, `{"labels":{"env":"d","team":"p"}}`},
		"array":  {[]any{1.5, nil, true}, `[1.5,null,true]`},
	} {
		b, err := encode.JSON(c.in)
		if err != nil || string(b) != c.want {
			t.Errorf("%s: JSON = %s, %v; want %s", name, b, err, c.want)
		}
		if bytes.HasSuffix(b, []byte("\n")) {
			t.Errorf("%s: trailing newline", name)
		}
	}
}

// JSON(x) read back with UseNumber is x, numbers as the text they were written as.
func TestJSONRoundTrips(t *testing.T) {
	for _, in := range []any{
		map[string]any{"projectNumber": json.Number("9007199254740993"), "labels": map[string]any{"env": "dev"},
			"list": []any{json.Number("1"), "two", nil, false, map[string]any{"deep": json.Number("0.5")}}},
		[]any{},
		map[string]any{},
		"plain",
	} {
		b, err := encode.JSON(in)
		if err != nil {
			t.Fatal(err)
		}
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.UseNumber()
		var back any
		if err := dec.Decode(&back); err != nil {
			t.Fatalf("%s: %v", b, err)
		}
		if !reflect.DeepEqual(back, in) {
			t.Errorf("round trip of %v gave %v", in, back)
		}
	}
}

func TestDeterministic(t *testing.T) {
	m := map[string]any{}
	for _, k := range []string{"q", "b", "z", "a", "m", "c"} {
		m[k] = k
	}
	first, _ := encode.JSON(m)
	for range 50 {
		if again, _ := encode.JSON(m); !bytes.Equal(first, again) {
			t.Fatalf("%s then %s", first, again)
		}
	}
}

func TestAsInt64AndNonFinite(t *testing.T) {
	if i, ok := encode.AsInt64("9007199254740993"); !ok || i != 9007199254740993 {
		t.Errorf("AsInt64 = %d, %v", i, ok)
	}
	if _, ok := encode.AsInt64("1.5"); ok {
		t.Error("1.5 read as an int64")
	}
	if _, ok := encode.AsInt64("99999999999999999999"); ok {
		t.Error("an integer beyond int64 read as one")
	}
	if !encode.NonFinite(map[string]any{"a": []any{math.Inf(-1)}}) || encode.NonFinite(map[string]any{"a": 1.0}) {
		t.Error("NonFinite misjudged")
	}
}
