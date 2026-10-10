package sqlfn

import (
	"math"
	"sort"
	"strconv"
	"strings"
)

// A Postgres catalogue function: the call resolves the overload Postgres would pick for the
// arguments' types, coerces each argument to the type that overload takes as Postgres's implicit
// coercion would, and runs that overload's implementation. A strict overload given a NULL is NULL
// without running.

// pgImpl is one overload's implementation, given arguments already of its declared types.
type pgImpl func(args []any) (any, error)

// pgImpls maps an overload's signature, name(argtypes), to its implementation. An overload Postgres
// has and the catalogue does not implement reports so when chosen.
var pgImpls = map[string]pgImpl{}

// pgRows is a set-returning implementation's result: one value per row, a []any per row for a
// function with OUT columns.
type pgRows [][]any

func registerPg(sig string, f pgImpl) {
	if _, dup := pgImpls[sig]; dup {
		panic("sqlfn: " + sig + " registered twice")
	}
	pgImpls[sig] = f
}

// pgEnv is what a catalogue's functions read beyond their arguments: the server's C library and
// build, and the clock.
type pgEnv struct {
	libm     pgLibmFuncs
	contract bool // the server's build fuses a*b+c (aarch64)
	clock    Clock
}

// decode is what parsing date and time input reads: the statement's time, and the build.
func (e pgEnv) decode() decodeEnv {
	return decodeEnv{now: pgTimestamptzOf(e.clock.Now()), contract: e.contract}
}

// pgEnvImpls are the implementations that read the environment.
var pgEnvImpls = map[string]func(pgEnv, []any) (any, error){}

func registerPgEnv(sig string, f func(pgEnv, []any) (any, error)) {
	if _, dup := pgImpls[sig]; dup {
		panic("sqlfn: " + sig + " registered twice")
	}
	if _, dup := pgEnvImpls[sig]; dup {
		panic("sqlfn: " + sig + " registered twice")
	}
	pgEnvImpls[sig] = f
}

// pgImplFor is the implementation of an overload in env.
func pgImplFor(sig string, env pgEnv) (pgImpl, bool) {
	if f, ok := pgEnvImpls[sig]; ok {
		return func(a []any) (any, error) { return f(env, a) }, true
	}
	f, ok := pgImpls[sig]
	return f, ok
}

// pgFunctions is the Postgres catalogue: every name in pgProcs with an implementation.
func pgFunctions(env pgEnv) []Func {
	names := make([]string, 0, len(pgProcs))
	for n := range pgProcs {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []Func
	for _, name := range names {
		procs := pgProcs[name]
		lo, hi := math.MaxInt, 0
		retset := false
		for _, p := range procs {
			n := len(p.args)
			if p.variadic != "" {
				hi = -1
			} else if hi >= 0 && n > hi {
				hi = n
			}
			lo = min(lo, n-len(p.defaults))
			retset = retset || p.retset
		}
		call := pgCall(name, env)
		if retset {
			out = append(out, NewTable(name, pgColumns(name, procs), lo, hi, func(a []any) ([]map[string]any, error) {
				v, err := call(a)
				if err != nil || v == nil {
					return nil, err
				}
				return v.([]map[string]any), nil
			}))
			continue
		}
		out = append(out, NewScalar(name, lo, hi, call))
	}
	return out
}

// pgColumns is a set-returning function's output columns: its OUT parameters, or a single column
// named after the function, as Postgres names it. They are those of the overloads stackql's data
// can reach; unnest(tsvector)'s, say, are not unnest's.
func pgColumns(name string, procs []pgProc) []string {
	for _, p := range procs {
		if len(p.outs) > 0 && pgReachable(p) {
			return p.outs
		}
	}
	return []string{name}
}

// pgReachable reports whether an overload takes only types stackql's data, its literals and the
// catalogue's own results come in.
func pgReachable(p pgProc) bool {
	for _, t := range p.args {
		switch t {
		case "text", "int4", "int8", "numeric", "float8", "bool", "json", "jsonb", "_text", "_int4",
			"timestamp", "timestamptz", "interval", "date", "any", "anyelement", "anyarray",
			"anynonarray", "anycompatible", "anycompatiblearray", "anycompatiblenonarray":
		default:
			return false
		}
	}
	return true
}

func pgCall(name string, env pgEnv) func([]any) (any, error) {
	return func(a []any) (any, error) {
		a, err := pgPrepare(name, a)
		if err != nil {
			return nil, err
		}
		types := make([]string, len(a))
		for i, v := range a {
			types[i] = pgTypeNameOf(v)
		}
		c, err := resolvePg(name, types)
		if err != nil {
			return nil, err
		}
		impl, ok := pgImplFor(c.proc.signature(name), env)
		if !ok {
			return nil, pgErrorf("omnisdk does not implement %s", c.proc.signature(name))
		}
		// Every argument is coerced first — Postgres reads an untyped literal while parsing the
		// call — and only then does a NULL make a strict function's result NULL.
		args := make([]any, len(a))
		hasNull := false
		for i, v := range a {
			if v == nil {
				hasNull = true
				continue
			}
			cv, err := pgCoerce(env, v, c.args[i], types, c.args)
			if err != nil {
				return nil, err
			}
			args[i] = cv
		}
		if hasNull && c.proc.strict {
			return nil, nil
		}
		for i, lit := range c.fill {
			v, err := pgInput(env, lit, c.proc.args[len(c.args)+i])
			if err != nil {
				return nil, err
			}
			args = append(args, v)
		}
		if v := c.proc.variadic; v != "" && v != "any" {
			// A non-"any" variadic's arguments are passed to the function as one array.
			fixed := len(c.proc.args) - 1
			arr := make(pgTextArr, 0, len(args)-fixed)
			for _, e := range args[fixed:] {
				if e == nil {
					arr = append(arr, nil)
					continue
				}
				s, _ := pgTextOf(e)
				arr = append(arr, &s)
			}
			args = append(args[:fixed:fixed], arr)
		}
		out, err := impl(args)
		if err != nil || !c.proc.retset {
			return out, err
		}
		return pgRowMaps(name, c.proc, out.(pgRows)), nil
	}
}

func pgRowMaps(name string, p pgProc, rows pgRows) []map[string]any {
	cols := p.outs
	out := make([]map[string]any, len(rows))
	for i, r := range rows {
		m := make(map[string]any, len(r))
		if len(cols) == 0 {
			m[name] = r[0]
		} else {
			for j, c := range cols {
				m[c] = r[j]
			}
		}
		out[i] = m
	}
	return out
}

// pgPrepare applies what stackql's Postgres formatter does to a call before Postgres sees it
// (astformat.PostgresSelectExprsFormatter): it appends ::json to the first argument of
// json_extract_path_text, given a path, and of json_array_elements_text.
func pgPrepare(name string, a []any) ([]any, error) {
	if name == "json_extract_path_text" && len(a) > 1 || name == "json_array_elements_text" && len(a) >= 1 {
		j, err := pgJSONCast(a[0])
		if err != nil {
			return nil, err
		}
		a = append([]any{j}, a[1:]...)
	}
	return a, nil
}

// pgJSONCast is v::json: text and untyped literals read as json when the function reads them, json
// as it is, jsonb as its text; no other type has a cast to json.
func pgJSONCast(v any) (any, error) {
	switch x := pgNormalize(v).(type) {
	case nil:
		return nil, nil
	case string:
		return pgJSONText(x), nil
	case pgUnknown:
		return pgJSONText(x), nil
	case pgJSON, pgJSONText:
		return x, nil
	case pgJSONB:
		return pgJSON(x), nil
	}
	return nil, pgErrorf("cannot cast type %s to json", pgDisplayName(pgTypeNameOf(v)))
}

// pgJSONText is text to be read as json; json_in validates it on use.
type pgJSONText string

// pgCoerce coerces v to the type t an overload takes: an unknown literal through t's input
// function, a known type through its implicit cast. A polymorphic or "any" parameter takes v as it
// is, an unknown literal there being text.
func pgCoerce(env pgEnv, v any, t string, in, target []string) (any, error) {
	from := pgTypeNameOf(v)
	if x, ok := v.(pgJSONText); ok {
		j, err := pgJSONIn(string(x))
		if err != nil {
			return nil, err
		}
		v, from = j, "json"
	}
	if from == t {
		return pgNormalize(v), nil
	}
	if t == "any" || isPolymorphic(t) {
		if from == "unknown" {
			if t == "anyelement" || t == "anycompatible" || t == "anynonarray" || t == "anycompatiblenonarray" {
				if rt := resolvedPolymorphic(t, in, target); rt != "" && rt != "unknown" {
					return pgInput(env, pgTextOrEmpty(v), rt)
				}
			}
			return pgTextOrEmpty(v), nil
		}
		return pgNormalize(v), nil
	}
	if from == "unknown" {
		return pgInput(env, pgTextOrEmpty(v), t)
	}
	return pgCast(pgNormalize(v), from, t)
}

// resolvedPolymorphic is the type a polymorphic parameter takes in this call: that of its known
// arguments.
func resolvedPolymorphic(t string, in, target []string) string {
	var known []string
	for i, tt := range target {
		if tt == t && in[i] != "unknown" {
			known = append(known, in[i])
		}
	}
	if len(known) == 0 {
		return "text"
	}
	if strings.HasPrefix(t, "anycompatible") {
		ct, _ := selectCommonType(known)
		return ct
	}
	return known[0]
}

func pgTextOrEmpty(v any) string {
	s, _ := pgTextOf(v)
	return s
}

// pgNormalize turns a provider value into its Postgres value: a number to numeric, an object or
// array to the text of its JSON, as stackql stores them.
func pgNormalize(v any) any {
	switch x := v.(type) {
	case float64, float32:
		n, err := pgNumericOf(x)
		if err != nil {
			return v
		}
		return n
	case int:
		return int64(x)
	case int32:
		return pgInt4(x)
	case map[string]any, []any:
		s, _ := pgTextOf(x)
		return s
	case jsonText:
		return string(x)
	}
	return v
}

// pgInput is type t's input function applied to text s.
func pgInput(env pgEnv, s, t string) (any, error) {
	switch t {
	case "text", "varchar", "bpchar", "name":
		return s, nil
	case "int4", "int2":
		return pgInt4In(s, t)
	case "int8":
		return pgInt8In(s)
	case "numeric":
		return parseNumeric(s)
	case "float8", "float4":
		return pgFloat8In(s)
	case "bool":
		return pgBoolIn(s)
	case "json":
		return pgJSONIn(s)
	case "jsonb":
		return pgJSONBIn(s)
	case "_text":
		return pgTextArrayIn(s)
	case "timestamptz":
		return pgTimestamptzIn(env.decode(), s)
	case "timestamp":
		return pgTimestampIn(env.decode(), s)
	case "date":
		return pgDateIn(env.decode(), s)
	case "interval":
		return pgIntervalIn(env.decode(), s)
	}
	return nil, pgErrorf("omnisdk does not implement input of type %s", pgDisplayName(t))
}

// pgCast is the implicit cast from type from to type t.
func pgCast(v any, from, t string) (any, error) {
	switch t {
	case "int8":
		if x, ok := v.(pgInt4); ok {
			return int64(x), nil
		}
	case "numeric":
		return pgNumericOf(v)
	case "float8":
		switch x := v.(type) {
		case pgInt4:
			return pgFloat8(float64(x)), nil
		case int64:
			return pgFloat8(float64(x)), nil
		case pgNumeric:
			return pgFloat8(x.toFloat64()), nil
		}
	case "text", "varchar", "bpchar":
		if s, ok := v.(string); ok {
			return s, nil
		}
	case "timestamp":
		if d, ok := v.(pgDate); ok {
			return pgDateToTimestamp(d)
		}
	case "timestamptz":
		switch x := v.(type) {
		case pgDate:
			return pgDateToTimestamptz(x)
		case pgTimestamp:
			return pgTimestampToTimestamptz(x)
		}
	}
	return nil, pgErrorf("omnisdk does not implement the cast from %s to %s", pgDisplayName(from), pgDisplayName(t))
}

// int4in.
func pgInt4In(s, t string) (any, error) {
	name := "integer"
	if t == "int2" {
		name = "smallint"
	}
	n, err := pgStrToInt(s, name)
	if err != nil {
		return nil, err
	}
	lo, hi := int64(math.MinInt32), int64(math.MaxInt32)
	if t == "int2" {
		lo, hi = math.MinInt16, math.MaxInt16
	}
	if n < lo || n > hi {
		return nil, pgErrorf("value \"%s\" is out of range for type %s", s, name)
	}
	return pgInt4(n), nil
}

// int8in.
func pgInt8In(s string) (any, error) {
	return pgStrToInt(s, "bigint")
}

func pgStrToInt(s, name string) (int64, error) {
	t := strings.TrimFunc(s, func(r rune) bool { return r < 0x80 && isPgSpace(byte(r)) })
	digits := strings.TrimLeft(t, "+-")
	if len(t)-len(digits) > 1 || digits == "" || strings.IndexFunc(digits, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return 0, pgErrorf("invalid input syntax for type %s: \"%s\"", name, s)
	}
	n, err := strconv.ParseInt(t, 10, 64)
	if err != nil {
		return 0, pgErrorf("value \"%s\" is out of range for type %s", s, name)
	}
	return n, nil
}

// float8in.
func pgFloat8In(s string) (any, error) {
	t := strings.TrimFunc(s, func(r rune) bool { return r < 0x80 && isPgSpace(byte(r)) })
	switch strings.ToLower(t) {
	case "nan":
		return pgFloat8(math.NaN()), nil
	case "infinity", "+infinity", "inf", "+inf":
		return pgFloat8(math.Inf(1)), nil
	case "-infinity", "-inf":
		return pgFloat8(math.Inf(-1)), nil
	}
	if strings.Contains(t, "_") {
		return nil, pgErrorf("invalid input syntax for type double precision: \"%s\"", s)
	}
	u := strings.TrimLeft(t, "+-")
	if len(u) > 2 && (u[:2] == "0x" || u[:2] == "0X") && !strings.ContainsAny(u, "pP") {
		t += "p0" // strtod reads a hex float without an exponent
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil {
		if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange {
			return nil, pgErrorf("\"%s\" is out of range for type double precision", t)
		}
		return nil, pgErrorf("invalid input syntax for type double precision: \"%s\"", s)
	}
	return pgFloat8(f), nil
}

// boolin: a unique prefix of true, false, yes, no, on or off, or 1 or 0.
func pgBoolIn(s string) (any, error) {
	t := strings.ToLower(strings.TrimFunc(s, func(r rune) bool { return r < 0x80 && isPgSpace(byte(r)) }))
	prefixOf := func(word string) bool { return t != "" && strings.HasPrefix(word, t) }
	switch {
	case t == "1":
		return true, nil
	case t == "0":
		return false, nil
	case prefixOf("true"), prefixOf("yes"):
		return true, nil
	case prefixOf("false"), prefixOf("no"):
		return false, nil
	case len(t) >= 2 && prefixOf("on"):
		return true, nil
	case len(t) >= 2 && prefixOf("off"):
		return false, nil
	}
	return nil, pgErrorf("invalid input syntax for type boolean: \"%s\"", s)
}

// pgTextArrayIn is array_in for text[]: {elem,"quoted elem",NULL}, with backslash escapes.
func pgTextArrayIn(s string) (any, error) {
	bad := func() error { return pgErrorf("malformed array literal: \"%s\"", s) }
	i := 0
	skip := func() {
		for i < len(s) && isPgSpace(s[i]) {
			i++
		}
	}
	skip()
	if i >= len(s) || s[i] != '{' {
		return nil, bad()
	}
	i++
	var out pgTextArr
	skip()
	if i < len(s) && s[i] == '}' {
		i++
		skip()
		if i != len(s) {
			return nil, bad()
		}
		return pgTextArr{}, nil
	}
	for {
		skip()
		if i >= len(s) {
			return nil, bad()
		}
		var elem strings.Builder
		quoted := false
		if s[i] == '"' {
			quoted = true
			i++
			for {
				if i >= len(s) {
					return nil, bad()
				}
				c := s[i]
				if c == '\\' {
					i++
					if i >= len(s) {
						return nil, bad()
					}
					elem.WriteByte(s[i])
				} else if c == '"' {
					i++
					break
				} else {
					elem.WriteByte(c)
				}
				i++
			}
		} else {
			for i < len(s) && s[i] != ',' && s[i] != '}' {
				c := s[i]
				if c == '"' || c == '{' {
					return nil, bad()
				}
				if c == '\\' {
					i++
					if i >= len(s) {
						return nil, bad()
					}
					elem.WriteByte(s[i])
					i++
					continue
				}
				elem.WriteByte(c)
				i++
			}
		}
		e := elem.String()
		if !quoted {
			e = strings.TrimRight(e, " \t\n\r\v\f")
			if e == "" {
				return nil, bad()
			}
			if strings.EqualFold(e, "NULL") {
				out = append(out, nil)
				goto sep
			}
		}
		out = append(out, &e)
	sep:
		skip()
		if i >= len(s) {
			return nil, bad()
		}
		if s[i] == ',' {
			i++
			continue
		}
		if s[i] == '}' {
			i++
			skip()
			if i != len(s) {
				return nil, bad()
			}
			return out, nil
		}
		return nil, bad()
	}
}
