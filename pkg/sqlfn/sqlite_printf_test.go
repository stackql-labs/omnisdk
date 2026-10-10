package sqlfn_test

import (
	"database/sql"
	"math"
	"math/rand"
	"strconv"
	"testing"
)

// printfFormats cover every conversion with every flag, width and precision form, the argument
// forms (*, too few arguments), and the conversions printf() refuses.
var printfFormats = []any{
	nil, "", "plain", "%", "%%", "a%", "%d", "%i", "%u", "%5d", "%-5d|", "%05d", "%+d", "% d", "%,d",
	"%.3d", "%x", "%X", "%#x", "%#X", "%o", "%#o", "%p", "%r", "%5r", "%lld", "%ld", "%*d", "%-*d|",
	"%.*f", "%*.*f", "%f", "%.0f", "%.2f", "%#.0f", "%10.3f", "%-10.3f|", "%010.3f", "%+.1f", "% .1f",
	"%,.2f", "%e", "%E", "%.0e", "%.10e", "%g", "%G", "%.3g", "%#g", "%!g", "%!.17g", "%!.15g", "%.20g",
	"%!0.17g", "%010g", "%s", "%5s|", "%-5s|", "%.2s", "%!.2s", "%!5s|", "%z", "%q", "%Q", "%w", "%.2q",
	"%#q", "%#Q", "%c", "%5c|", "%.3c", "%-4.2c|", "%!c", "%n", "%T", "%S", "%y", "%k", "%d %s %f",
	"%d%d%d", "%.100f", "%.40e", "%5%", "%-%", "%l", "%ll", "%.", "%*", "%.*", "%1$d",
}

var printfArgs = []any{nil, int64(0), int64(-42), int64(math.MaxInt64), int64(math.MinInt64), 3.14159,
	-2.5, 1e20, 1.5e-10, 0.1, 255.0, math.Inf(1), math.Inf(-1), "", "héllo", "it's", "a\"b", "x\x01y\\",
	"12", "-7.5", []byte("bl\x00ob"), int64(3), int64(-3)}

func TestSQLiteParityPrintf(t *testing.T) {
	var args [][]any
	for _, f := range printfFormats {
		args = append(args, []any{f})
		for _, a := range printfArgs {
			args = append(args, []any{f, a}, []any{f, a, a}, []any{f, int64(8), a}, []any{f, int64(-8), int64(2), a})
		}
	}
	checkParity(t, sqliteCatalog(t), []parity{{name: "printf", args: args}, {name: "format", arity: []int{1, 2}, dom: small}})
}

// TestSQLiteRealText checks REAL text, JSON numbers, and text read as REAL, on doubles across the
// whole range: random bit patterns, decimal-looking values, powers of ten and their neighbours.
func TestSQLiteRealText(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cat := sqliteCatalog(t)
	rng := rand.New(rand.NewSource(1))
	var vals []float64
	for i := 0; i < 4000; i++ {
		f := math.Float64frombits(rng.Uint64())
		if math.IsNaN(f) {
			continue
		}
		vals = append(vals, f)
	}
	for e := -330; e <= 310; e++ {
		p := math.Pow(10, float64(e))
		vals = append(vals, p, math.Nextafter(p, 0), math.Nextafter(p, math.Inf(1)), 1.5*p, 9.999999999999999*p)
	}
	for i := 0; i < 2000; i++ {
		v, _ := strconv.ParseFloat(strconv.FormatFloat(rng.Float64()*1000, 'f', rng.Intn(12), 64), 64)
		vals = append(vals, v, -v)
	}
	vals = append(vals, 5e-324, 2.2250738585072014e-308, math.MaxFloat64, 0.1, 0.2, 0.3, 49.47, 1/3.0)
	cast, _ := cat.Get("printf")
	arr, _ := cat.Get("json_array")
	fails := 0
	for _, f := range vals {
		var text, js string
		var back float64
		if err := db.QueryRow("select cast(? as text), json_array(?), cast(cast(? as text) as real)", f, f, f).Scan(&text, &js, &back); err != nil {
			t.Fatal(err)
		}
		got, _ := cast.Call([]any{"%!.17g", f})
		gotJS, _ := arr.Call([]any{f})
		gotBack, _ := strconv.ParseFloat(text, 64)
		_ = gotBack
		if got != text || !same(js, gotJS) {
			fails++
			if fails < 10 {
				t.Errorf("%v (%x): text want %q got %q; json want %q got %v", f, math.Float64bits(f), text, got, js, gotJS)
			}
		}
	}
	// Text to REAL: SQLite's parse, including digits beyond what a uint64 holds.
	texts := []string{"0.30000000000000004441", "123456789012345678901234567890", "1.7976931348623157e308",
		"1.7976931348623159e308", "4.9e-324", "2.4703282292062327e-324", "2.4703282292062328e-324",
		"0.1e-400", "9" + string(make([]byte, 0)) + "99999999999999999999.5", " 12.5 ", "1e10000", "-0.0"}
	for i := 0; i < 2000; i++ {
		texts = append(texts, strconv.FormatFloat(math.Float64frombits(rng.Uint64()), 'e', 16+rng.Intn(10), 64))
	}
	rf, _ := cat.Get("round")
	for _, s := range texts {
		var want any
		if err := db.QueryRow("select cast(? as real)", s).Scan(&want); err != nil {
			continue
		}
		got, _ := rf.Call([]any{s, int64(30)})
		wantRound := want
		_ = db.QueryRow("select round(?, 30)", s).Scan(&wantRound)
		if !same(wantRound, got) {
			fails++
			if fails < 20 {
				t.Errorf("round(%q, 30): want %v got %v", s, wantRound, got)
			}
		}
	}
	if fails > 0 {
		t.Errorf("%d differ", fails)
	}
}
