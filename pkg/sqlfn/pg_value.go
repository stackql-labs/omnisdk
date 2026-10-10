package sqlfn

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// The Postgres catalogue's value model. Postgres is typed: which function a name means, and what it
// does, depends on its arguments' types. A row value arrives with the type stackql gives its column
// on a Postgres backend — a provider string is text, an integer bigint, a number numeric, a boolean
// boolean, and a JSON object or array the text of its JSON — and a function's result carries the
// Postgres type it returns, so a nested call resolves as Postgres would resolve it. Settle turns a
// result into a plain value as it leaves an expression.

// pgType is a Postgres type.
type pgType int

const (
	pgTypeUnknown pgType = iota
	pgTypeText
	pgTypeInt4
	pgTypeInt8
	pgTypeNumeric
	pgTypeFloat8
	pgTypeBool
	pgTypeJSON
	pgTypeJSONB
	pgTypeTextArray
	pgTypeInt4Array
	pgTypeTimestamptz
	pgTypeTimestamp
	pgTypeDate
	pgTypeInterval
)

var pgTypeNames = map[pgType]string{
	pgTypeUnknown: "unknown", pgTypeText: "text", pgTypeInt4: "integer", pgTypeInt8: "bigint",
	pgTypeNumeric: "numeric", pgTypeFloat8: "double precision", pgTypeBool: "boolean", pgTypeJSON: "json",
	pgTypeJSONB: "jsonb", pgTypeTextArray: "text[]", pgTypeInt4Array: "integer[]",
	pgTypeTimestamptz: "timestamp with time zone", pgTypeTimestamp: "timestamp without time zone",
	pgTypeDate: "date", pgTypeInterval: "interval",
}

func (t pgType) String() string { return pgTypeNames[t] }

// Typed Postgres values. A Go string is text, int64 bigint, bool boolean; float64 and json.Number
// from a provider are numeric, as stackql types a provider number.
type (
	pgInt4    int32
	pgFloat8  float64
	pgJSON    string // json: the text exactly as written
	pgJSONB   string // jsonb: Postgres's normalized rendering
	pgTextArr []*string
	pgInt4Arr []*int32
)

// pgUnknown is an untyped string literal, which Postgres types by what the function it is given to
// wants.
type pgUnknown string

// pgTypeOf is v's Postgres type; nil is unknown, as an untyped NULL is.
func pgTypeOf(v any) pgType {
	switch v.(type) {
	case nil, pgUnknown:
		return pgTypeUnknown
	case string, jsonText:
		return pgTypeText
	case pgInt4, int32:
		return pgTypeInt4
	case int64, int:
		return pgTypeInt8
	case pgNumeric, float64, float32, json.Number:
		return pgTypeNumeric
	case pgFloat8:
		return pgTypeFloat8
	case bool:
		return pgTypeBool
	case pgJSON, pgJSONText:
		return pgTypeJSON
	case pgJSONB:
		return pgTypeJSONB
	case pgTimestamptz:
		return pgTypeTimestamptz
	case pgTimestamp:
		return pgTypeTimestamp
	case pgDate:
		return pgTypeDate
	case pgInterval:
		return pgTypeInterval
	case pgTextArr:
		return pgTypeTextArray
	case pgInt4Arr:
		return pgTypeInt4Array
	case map[string]any, []any:
		return pgTypeText
	}
	return pgTypeText
}

// pgTextOf is a value's text output, as Postgres's type output function renders it.
func pgTextOf(v any) (string, bool) {
	switch x := v.(type) {
	case nil:
		return "", false
	case string:
		return x, true
	case pgUnknown:
		return string(x), true
	case jsonText:
		return string(x), true
	case pgJSONText:
		return string(x), true
	case pgTimestamptz, pgTimestamp, pgDate, pgInterval:
		return x.(interface{ String() string }).String(), true
	case pgInt4:
		return strconv.FormatInt(int64(x), 10), true
	case int32:
		return strconv.FormatInt(int64(x), 10), true
	case int64:
		return strconv.FormatInt(x, 10), true
	case int:
		return strconv.Itoa(x), true
	case bool:
		if x {
			return "true", true
		}
		return "false", true
	case pgFloat8:
		return pgFloat8Text(float64(x)), true
	case pgNumeric:
		return x.String(), true
	case float64, float32, json.Number:
		n, err := pgNumericOf(v)
		if err != nil {
			return "", false
		}
		return n.String(), true
	case pgJSON:
		return string(x), true
	case pgJSONB:
		return string(x), true
	case pgTextArr:
		return pgArrayText(x), true
	case pgInt4Arr:
		out := make([]*string, len(x))
		for i, e := range x {
			if e != nil {
				s := strconv.FormatInt(int64(*e), 10)
				out[i] = &s
			}
		}
		return pgArrayText(out), true
	case map[string]any, []any:
		b, err := json.Marshal(x)
		if err != nil {
			return "", false
		}
		return string(b), true
	}
	return fmt.Sprint(v), true
}

// pgBoolText is how a boolean prints through ::text — "true"/"false" — which is what reaches a text
// argument; psql's t/f is display only.

// pgFloat8Text is float8 output with extra_float_digits 1 (Postgres 12+ default): the shortest text
// that reads back exactly, in %g-style layout with an exponent past 15 digits or below 1e-4.
func pgFloat8Text(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	case f == 0:
		if math.Signbit(f) {
			return "-0"
		}
		return "0"
	}
	// Postgres's float8out uses the Ryu shortest digits, printed as %.17g would lay them out: an
	// exponent when the decimal exponent is below -4 or at least 15.
	s := strconv.FormatFloat(f, 'e', -1, 64)
	mant, expPart, _ := strings.Cut(s, "e")
	exp, _ := strconv.Atoi(expPart)
	neg := strings.HasPrefix(mant, "-")
	mant = strings.TrimPrefix(mant, "-")
	digits := strings.Replace(mant, ".", "", 1)
	var out string
	if exp < -4 || exp >= 15 {
		m := digits[:1]
		if len(digits) > 1 {
			m += "." + digits[1:]
		}
		sign := "+"
		if exp < 0 {
			sign, exp = "-", -exp
		}
		e := strconv.Itoa(exp)
		if len(e) < 2 {
			e = "0" + e
		}
		out = m + "e" + sign + e
	} else if exp < 0 {
		out = "0." + strings.Repeat("0", -exp-1) + digits
	} else if exp+1 >= len(digits) {
		out = digits + strings.Repeat("0", exp+1-len(digits))
	} else {
		out = digits[:exp+1] + "." + digits[exp+1:]
	}
	if neg {
		out = "-" + out
	}
	return out
}

// pgArrayText is an array's text output: {a,b,"c d",NULL}.
func pgArrayText(a []*string) string {
	var b strings.Builder
	b.WriteByte('{')
	for i, e := range a {
		if i > 0 {
			b.WriteByte(',')
		}
		if e == nil {
			b.WriteString("NULL")
			continue
		}
		s := *e
		quote := s == "" || strings.EqualFold(s, "NULL")
		for _, c := range []byte(s) {
			if c == '"' || c == '\\' || c == '{' || c == '}' || c == ',' || c == ' ' || (c >= '\t' && c <= '\r') {
				quote = true
				break
			}
		}
		if !quote {
			b.WriteString(s)
			continue
		}
		b.WriteByte('"')
		for _, c := range []byte(s) {
			if c == '"' || c == '\\' {
				b.WriteByte('\\')
			}
			b.WriteByte(c)
		}
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

// errPg is a Postgres error: its message as Postgres words it.
type errPg struct{ msg string }

func (e errPg) Error() string { return e.msg }

func pgErrorf(format string, a ...any) error { return errPg{msg: fmt.Sprintf(format, a...)} }

// noFunction is Postgres's error for a call no signature fits.
func noFunction(name string, args []any) error {
	types := make([]string, len(args))
	for i, a := range args {
		types[i] = pgTypeOf(a).String()
	}
	return pgErrorf("function %s(%s) does not exist", name, strings.Join(types, ", "))
}

// settlePg is a Postgres value as it leaves an expression: text, int64, bool, float64 for float8, a
// json.Number for numeric (exact, as Postgres prints it), and text for JSON, arrays and times.
func settlePg(v any) any {
	switch x := v.(type) {
	case pgInt4:
		return int64(x)
	case pgFloat8:
		return float64(x)
	case pgNumeric:
		return json.Number(x.String())
	case pgJSON:
		return string(x)
	case pgJSONB:
		return string(x)
	case pgTextArr, pgInt4Arr, pgTimestamptz, pgTimestamp, pgDate, pgInterval:
		s, _ := pgTextOf(x)
		return s
	}
	return v
}
