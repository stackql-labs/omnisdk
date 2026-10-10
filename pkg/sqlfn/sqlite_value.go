package sqlfn

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
)

// SQLite's value model, which the SQLite catalogue follows exactly: every value is NULL, INTEGER
// (int64), REAL (float64), TEXT (string) or BLOB ([]byte). A Go bool is the INTEGER 1 or 0 — SQLite
// has no boolean — and a JSON object or array from a provider row is the TEXT of its JSON, as stackql
// stores it. Conversions between classes are SQLite's own, reproduced here and pinned against the
// SQLite build stackql embeds.

// class is a value's storage class after the provider-row mapping above.
type class int

const (
	classNull class = iota
	classInteger
	classReal
	classText
	classBlob
)

// norm maps a value onto SQLite's classes.
func norm(v any) (class, any) {
	switch x := v.(type) {
	case nil:
		return classNull, nil
	case bool:
		if x {
			return classInteger, int64(1)
		}
		return classInteger, int64(0)
	case int:
		return classInteger, int64(x)
	case int32:
		return classInteger, int64(x)
	case int64:
		return classInteger, x
	case float32:
		return classReal, float64(x)
	case float64:
		return classReal, x
	case string:
		return classText, x
	case jsonText:
		return classText, string(x)
	case []byte:
		return classBlob, x
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return classInteger, i
		}
		f, _ := x.Float64()
		return classReal, f
	}
	b, err := json.Marshal(v)
	if err != nil {
		return classText, ""
	}
	return classText, string(b)
}

// sqlText is a value as TEXT, as sqlite3_value_text renders it. NULL is the empty string; callers
// that must distinguish NULL test for it first.
func sqlText(v any) string {
	c, x := norm(v)
	switch c {
	case classInteger:
		return strconv.FormatInt(x.(int64), 10)
	case classReal:
		return realText(x.(float64))
	case classText:
		return x.(string)
	case classBlob:
		return string(x.([]byte))
	}
	return ""
}

// sqlBytes is a value as SQLite reads its bytes: a BLOB as is, anything else its TEXT.
func sqlBytes(v any) []byte {
	if c, x := norm(v); c == classBlob {
		return x.([]byte)
	}
	return []byte(sqlText(v))
}

// realText renders a REAL as SQLite does: "%!.17g" through SQLite's own printf, so 17 significant
// digits or fewer where fewer read back as the same value, always with a point or an exponent.
func realText(f float64) string { return formatFloat("%!.17g", f) }

// numericPrefixValue reads text as SQLite's sqlite3AtoF reads it for a REAL: surrounding
// whitespace, then the longest leading number, 0 where there is none.
func numericPrefixValue(s string) float64 {
	f, _ := sqliteAtoFRaw(s)
	return f
}

// sqlReal is a value as sqlite3_value_double reads it.
func sqlReal(v any) float64 {
	c, x := norm(v)
	switch c {
	case classInteger:
		return float64(x.(int64))
	case classReal:
		return x.(float64)
	case classText, classBlob:
		return numericPrefixValue(sqlText(x))
	}
	return 0
}

// sqlInt is a value as sqlite3_value_int64 reads it: a REAL truncated toward zero and clamped; text
// by its leading integer only — "12.5" and "1e3" read as 12 and 1 — saturating beyond 64 bits.
func sqlInt(v any) int64 {
	c, x := norm(v)
	switch c {
	case classInteger:
		return x.(int64)
	case classReal:
		return realToInt(x.(float64))
	case classText, classBlob:
		s := strings.TrimLeft(sqlText(x), " \t\n\r\f\v")
		end := 0
		if end < len(s) && (s[end] == '+' || s[end] == '-') {
			end++
		}
		start := end
		for end < len(s) && s[end] >= '0' && s[end] <= '9' {
			end++
		}
		if end == start {
			return 0
		}
		if i, err := strconv.ParseInt(s[:end], 10, 64); err == nil {
			return i
		}
		if s[0] == '-' {
			return math.MinInt64
		}
		return math.MaxInt64
	}
	return 0
}

func realToInt(f float64) int64 {
	switch {
	case math.IsNaN(f):
		return 0
	case f <= -9223372036854775808.0:
		return math.MinInt64
	case f >= 9223372036854775807.0:
		return math.MaxInt64
	}
	return int64(f)
}

// numericClass is sqlite3_value_numeric_type: a value as INTEGER or REAL where it is one, or is text
// that is wholly a number (surrounding whitespace allowed) — an integer literal as INTEGER, any other
// number as REAL; otherwise its own class.
func numericClass(v any) (class, any) {
	c, x := norm(v)
	if c != classText {
		return c, x
	}
	s := strings.TrimSpace(x.(string))
	if s == "" {
		return c, x
	}
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		return classInteger, i
	}
	if f, rc := sqliteAtoFRaw(s); rc > 0 {
		return classReal, f
	}
	return c, x
}
