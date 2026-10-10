package sqlfn_test

import (
	"math"
	"math/rand"
	"strings"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/sqlfn"
)

// pgToCharNumFormats are pictures across every keyword and modifier, alone and combined.
var pgToCharNumFormats = []string{
	"", " ", "x", "9", "99", "999", "9999", "99999", "999999999999", "9999999999999999999999",
	"0", "00", "0000", "09", "90", "0099", "9900", "0909", "99099",
	"9.9", "99.99", "999.999", "0.0", "00.00", "9.00", "0.99", ".99", "9.", ".", "99.", "99.0", "0.", "9.090",
	"99D99", "D99", "9D", "d999", "9,999", ",999", "9,9,9", "0,000", "9G999.99", "G999", "g999",
	"999,999,999.99", "FM9999", "FM99.99", "FM0.00", "FM9.9", "FM9.0099", "FM00.0099", "FM.999",
	"FM9,999.99", "FM,999", "FM9.", "FM0.", "FM90.0900", "fm999", "Fm999", "FM9G999",
	"S999", "999S", "S9.99", "9.99S", "S0.00", "FMS999", "S 999", "999 S", "s999", "9S99", "99.9S9",
	"FM999S", "FMS9.99", "S000", "S9999.99", "9.S99",
	"MI999", "999MI", "9.99MI", "FM999MI", "mi999", "9MI99", "PL999", "999PL", "9.99PL", "FMPL999",
	"FM999PL", "SG999", "999SG", "FMSG999", "99 SG", "9.9SG", "SGMI999", "PLMI999", "MIPL9.9",
	"999PR", "9.99PR", "FM999PR", "0000PR", "FM9.99PR", "pr", "PR",
	"L999", "L9.99", "FML999", "999L", "l9.9",
	"99V9", "99V99", "9V999", "V99", "V", "9V", "FM99V9", "99V9S", "0V00", "99V9PR", "V9.",
	"9EEEE", "9.9EEEE", "9.99eeee", "0.000EEEE", "99.99EEEE", ".9EEEE", "9.EEEE", "EEEE",
	"\"x\"9.9EEEE\"y\"", "999.999999999999999999EEEE", "EEE9",
	"RN", "rn", "FMRN", "fmrn", "RNSG", "MIRN", "RN th", "RNTH", "RN999", "9RN", "RN.99", "RNRN",
	"PLRN", "RNPR", "SRN", "RN S",
	"999TH", "999th", "FM999th", "9TH", "99.9TH", "TH", "th", "9TH9", "999SPth", "9 TH", "tH9",
	"999thPR", "S999TH", "999MITH",
	"B999", "B9.99", "999B", "C999", "SP999", "c9", "b0.0",
	"\"abc\"999", "999\"x\"", "9 9 9", "a9b9", "$999", "9-9", "é999", "\\\"999", "\"a\\\"b\"9",
	"9999 \"€\"", "\"9\"9", "\"", "\"unterminated", "9\\9", "99%",
}

// pgToCharNumErrors are pictures Postgres rejects.
var pgToCharNumErrors = []string{
	"9.9.9", "99D9.9", "D.9", "9V9.9", "9.9V9", "V.", "S999S", "S999MI", "MI999S", "PL999S", "SG999S",
	"999PR9", "999PR0", "S999PR", "999PRS", "MI999PR", "PR999MI", "9EEEE9", "9EEEEEEEE", "FM9EEEE",
	"S9EEEE", "RN9EEEE", "9V9EEEE", "B9EEEE", "9.9EEEE9", "9EEEE.", "PL9EEEE", "9MIEEEE", "PR9EEEE",
	"9EEEERN", "9EEEEFM",
}

func pgToCharNumCases(values []any, formats []string) []pgCase {
	var cases []pgCase
	for _, v := range values {
		for _, f := range formats {
			cases = append(cases, pgCase{"to_char", []any{v, f}})
		}
	}
	return cases
}

var (
	pgToCharNumNumerics = []any{0.0, 1.0, -1.0, 5.0, 12.0, 13.0, 21.0, 111.0, 112.0, 123.0, 1234.5678,
		-1234.5678, 0.5, -0.5, 0.05, 0.001, 9.995, 99.995, -0.004, 2.5, -2.5, 3.5, 0.15, 1e20, -1e20,
		1e-20, 3999.0, 3999.5, 4000.0, 0.4, -12.0, 99.99, 999.9999, 1e300, math.NaN(), math.Inf(1),
		math.Inf(-1), sqlfn.PgUnknown("123456789012345678901234567890.123456789")}
	pgToCharNumInt4s = []any{sqlfn.PgInt4(0), sqlfn.PgInt4(1), sqlfn.PgInt4(-1), sqlfn.PgInt4(7),
		sqlfn.PgInt4(12), sqlfn.PgInt4(13), sqlfn.PgInt4(21), sqlfn.PgInt4(-42), sqlfn.PgInt4(3999),
		sqlfn.PgInt4(4000), sqlfn.PgInt4(123456), sqlfn.PgInt4(math.MaxInt32), sqlfn.PgInt4(math.MinInt32)}
	pgToCharNumInt8s = []any{int64(0), int64(1), int64(-1), int64(42), int64(-7), int64(3999),
		int64(1234567890123), int64(math.MaxInt64), int64(math.MinInt64), int64(2147483648),
		int64(-2147483648), int64(112)}
	pgToCharNumFloat8s = []any{sqlfn.PgFloat8(0), sqlfn.PgFloat8(math.Copysign(0, -1)), sqlfn.PgFloat8(1),
		sqlfn.PgFloat8(-1), sqlfn.PgFloat8(0.5), sqlfn.PgFloat8(1.5), sqlfn.PgFloat8(2.5),
		sqlfn.PgFloat8(-2.5), sqlfn.PgFloat8(0.125), sqlfn.PgFloat8(0.375), sqlfn.PgFloat8(0.0005),
		sqlfn.PgFloat8(1.005), sqlfn.PgFloat8(2.675), sqlfn.PgFloat8(1234.5678), sqlfn.PgFloat8(-1234.5678),
		sqlfn.PgFloat8(1e15), sqlfn.PgFloat8(1e16), sqlfn.PgFloat8(123456789012345.6),
		sqlfn.PgFloat8(1e300), sqlfn.PgFloat8(1e-310), sqlfn.PgFloat8(math.NaN()),
		sqlfn.PgFloat8(math.Inf(1)), sqlfn.PgFloat8(math.Inf(-1)), sqlfn.PgFloat8(3999.5),
		sqlfn.PgFloat8(4000), sqlfn.PgFloat8(12), sqlfn.PgFloat8(-0.001), sqlfn.PgFloat8(0.1),
		sqlfn.PgFloat8(math.MaxFloat64), sqlfn.PgFloat8(13), sqlfn.PgFloat8(9.995)}
)

// TestPgParityToCharNum runs to_char over numeric, int4, int8 and float8 across the pictures.
func TestPgParityToCharNum(t *testing.T) {
	postgresImage(t)
	formats := append(append([]string{}, pgToCharNumFormats...), pgToCharNumErrors...)
	var cases []pgCase
	for _, vs := range [][]any{pgToCharNumNumerics, pgToCharNumInt4s, pgToCharNumInt8s, pgToCharNumFloat8s} {
		cases = append(cases, pgToCharNumCases(vs, formats)...)
	}
	checkPg(t, cases)
}

// TestPgParityToCharNumWide runs the pictures whose width or precision is extreme: long EEEE
// mantissas, many V digits, decimals past numeric's rounding limit and float8's DBL_DIG cap.
func TestPgParityToCharNumWide(t *testing.T) {
	postgresImage(t)
	nines := func(n int) string { return strings.Repeat("9", n) }
	formats := []string{
		"9." + nines(20) + "EEEE", "9." + nines(400) + "EEEE", "9." + nines(17) + "EEEE", "9." + nines(30),
		"9V" + nines(8), "9V" + nines(9), "9V" + nines(10), "9V" + nines(12), "9V" + nines(18), "9V" + nines(19),
		"9V" + nines(30), nines(40) + "V" + nines(5), "9." + nines(2100), "FM9." + nines(2100), nines(30) + ".99",
		nines(20) + "." + nines(20), "FM" + nines(20) + "." + nines(20), "0." + strings.Repeat("0", 40),
	}
	var cases []pgCase
	for _, vs := range [][]any{pgToCharNumNumerics, pgToCharNumInt4s, pgToCharNumInt8s, pgToCharNumFloat8s} {
		cases = append(cases, pgToCharNumCases(vs, formats)...)
	}
	checkPg(t, cases)
}

// TestPgParityToCharNumErrors checks the catalogue fails with Postgres's message.
func TestPgParityToCharNumErrors(t *testing.T) {
	db := postgresImage(t)
	cat := pgCatalog(t)
	fn, _ := cat.Get("to_char")
	var cases []pgCase
	for _, vs := range [][]any{pgToCharNumNumerics, pgToCharNumInt4s, pgToCharNumInt8s, pgToCharNumFloat8s} {
		cases = append(cases, pgToCharNumCases(vs, append(append([]string{}, pgToCharNumErrors...),
			"RN", "999th", "9V"+strings.Repeat("9", 19)))...)
	}
	for _, c := range cases {
		var s string
		werr := db.QueryRow("select " + pgCallSQL(c.name, c.args)).Scan(&s)
		_, gerr := fn.Call(c.args)
		if werr == nil || gerr == nil {
			continue
		}
		want := strings.TrimPrefix(werr.Error(), "ERROR: ")
		if i := strings.Index(want, " (SQLSTATE"); i >= 0 {
			want = want[:i]
		}
		if want != gerr.Error() {
			t.Errorf("%s: want error %q, got %q", pgCallSQL(c.name, c.args), want, gerr.Error())
		}
	}
}

// TestPgParityToCharNumRandom runs pictures drawn at random, from a fixed seed, from the keywords
// and some literal characters, over a value of each type and sign.
func TestPgParityToCharNumRandom(t *testing.T) {
	postgresImage(t)
	tokens := []string{"9", "9", "9", "0", "0", ".", "D", ",", "G", "S", "MI", "PL", "SG", "PR", "FM",
		"L", "V", "RN", "rn", "TH", "th", "B", "EEEE", " ", "x", "\"q\""}
	r := rand.New(rand.NewSource(1))
	values := []any{1234.5678, -0.05, 0.0, 7.0, math.NaN(), sqlfn.PgInt4(-12), sqlfn.PgInt4(3),
		int64(1000001), int64(-1), sqlfn.PgFloat8(-98.765), sqlfn.PgFloat8(0.5), sqlfn.PgFloat8(math.Inf(1))}
	for _, vs := range [][]any{pgToCharNumNumerics, pgToCharNumInt4s, pgToCharNumInt8s, pgToCharNumFloat8s} {
		values = append(values, vs...)
	}
	var cases []pgCase
	for i := 0; i < 20000; i++ {
		var b strings.Builder
		for n := 1 + r.Intn(8); n > 0; n-- {
			b.WriteString(tokens[r.Intn(len(tokens))])
		}
		cases = append(cases, pgCase{"to_char", []any{values[r.Intn(len(values))], b.String()}})
	}
	checkPg(t, cases)
}
