package sqlfn

import (
	"math"
	"sort"
	"strconv"
	"strings"
)

// Postgres's json and jsonb functions, ported from json.c, jsonb.c, jsonb_util.c and jsonfuncs.c.
// json is text kept exactly as written — its functions return slices of the source — and jsonb is a
// parsed value with object keys deduplicated (the last wins) and ordered by length then bytes.

// jbValue is a jsonb value (JsonbValue).
type jbValue struct {
	kind jbKind
	str  string
	num  pgNumeric
	b    bool
	arr  []jbValue
	keys []string // an object's keys, in jsonb order
	vals []jbValue
	raw  bool // a top-level scalar, stored as a one-element "raw scalar" array
}

type jbKind int

const (
	jbNull jbKind = iota
	jbString
	jbNumeric
	jbBool
	jbArray
	jbObject
)

// pgSemErr carries an error raised inside a parser callback out of the parse.
type pgSemErr struct{ err error }

// runSem runs a parse whose callbacks may raise, as ereport does.
func runSem(l *pgJSONLex, sem *pgJSONSem) (err error) {
	defer func() {
		if r := recover(); r != nil {
			se, ok := r.(pgSemErr)
			if !ok {
				panic(r)
			}
			err = se.err
		}
	}()
	if e := l.parse(sem); e != jeSuccess {
		return l.errorFor(e)
	}
	return nil
}

func raise(err error) { panic(pgSemErr{err: err}) }

// pgJSONBIn is jsonb_in.
func pgJSONBIn(s string) (any, error) {
	v, err := parseJSONB(s)
	if err != nil {
		return nil, err
	}
	return pgJSONB(jsonbOut(v, false)), nil
}

// parseJSONB is jsonb_in's parse into a value.
func parseJSONB(s string) (jbValue, error) {
	l := newPgJSONLex(s, true)
	type frame struct {
		v   jbValue
		key string
		ord []int
	}
	var stack []*frame
	var result jbValue
	push := func(v jbValue) {
		if len(stack) == 0 {
			result = v
			return
		}
		top := stack[len(stack)-1]
		if top.v.kind == jbArray {
			top.v.arr = append(top.v.arr, v)
		} else {
			top.v.keys = append(top.v.keys, top.key)
			top.v.vals = append(top.v.vals, v)
		}
	}
	sem := &pgJSONSem{
		objectStart: func() { stack = append(stack, &frame{v: jbValue{kind: jbObject}}) },
		arrayStart:  func() { stack = append(stack, &frame{v: jbValue{kind: jbArray}}) },
		objectEnd: func() {
			top := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			push(uniqueify(top.v))
		},
		arrayEnd: func() {
			top := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			push(top.v)
		},
		objectFieldStart: func(fname string, _ bool) { stack[len(stack)-1].key = fname },
		scalar: func(tok string, t pgJSONToken) {
			v, err := jbScalar(tok, t)
			if err != nil {
				raise(err)
			}
			if len(stack) == 0 {
				v.raw = true
			}
			push(v)
		},
	}
	if err := runSem(l, sem); err != nil {
		return jbValue{}, err
	}
	return result, nil
}

func jbScalar(tok string, t pgJSONToken) (jbValue, error) {
	switch t {
	case jtString:
		return jbValue{kind: jbString, str: tok}, nil
	case jtNumber:
		n, err := parseNumeric(tok)
		if err != nil {
			return jbValue{}, err
		}
		return jbValue{kind: jbNumeric, num: n}, nil
	case jtTrue:
		return jbValue{kind: jbBool, b: true}, nil
	case jtFalse:
		return jbValue{kind: jbBool}, nil
	}
	return jbValue{kind: jbNull}, nil
}

// lengthCompare orders jsonb keys: shorter first, then bytes.
func lengthCompare(a, b string) int {
	if len(a) != len(b) {
		if len(a) < len(b) {
			return -1
		}
		return 1
	}
	return strings.Compare(a, b)
}

// uniqueify is uniqueifyJsonbObject: keys sorted, a duplicate's last value kept.
func uniqueify(o jbValue) jbValue {
	type pair struct {
		k     string
		v     jbValue
		order int
	}
	ps := make([]pair, len(o.keys))
	for i := range o.keys {
		ps[i] = pair{o.keys[i], o.vals[i], i}
	}
	sort.SliceStable(ps, func(i, j int) bool {
		if c := lengthCompare(ps[i].k, ps[j].k); c != 0 {
			return c < 0
		}
		return ps[i].order > ps[j].order
	})
	out := jbValue{kind: jbObject}
	for i, p := range ps {
		if i > 0 && p.k == ps[i-1].k {
			continue
		}
		out.keys = append(out.keys, p.k)
		out.vals = append(out.vals, p.v)
	}
	return out
}

// jsonbOut is JsonbToCStringWorker: compact (", " and ": ") or, with indent, as jsonb_pretty prints.
func jsonbOut(v jbValue, indent bool) string {
	var b strings.Builder
	sep := ", "
	if indent {
		sep = ","
	}
	addIndent := func(on bool, level int) {
		if on {
			b.WriteByte('\n')
			for i := 0; i < level; i++ {
				b.WriteString("    ")
			}
		}
	}
	// The iterator's token stream, walked as the worker walks it.
	first := true
	level := 0
	useIndent := false
	lastWasKey := false
	var walk func(v jbValue, asValue bool)
	scalarOut := func(v jbValue) {
		switch v.kind {
		case jbNull:
			b.WriteString("null")
		case jbString:
			pgEscapeJSON(&b, v.str)
		case jbNumeric:
			b.WriteString(v.num.String())
		case jbBool:
			if v.b {
				b.WriteString("true")
			} else {
				b.WriteString("false")
			}
		}
	}
	walk = func(v jbValue, afterKey bool) {
		switch v.kind {
		case jbArray:
			if !first {
				b.WriteString(sep)
			}
			addIndent(useIndent && !afterKey, level)
			b.WriteByte('[')
			first = true
			level++
			useIndent, lastWasKey = indent, false
			for _, e := range v.arr {
				if e.kind == jbArray || e.kind == jbObject {
					walk(e, false)
					continue
				}
				if !first {
					b.WriteString(sep)
				}
				first = false
				addIndent(useIndent, level)
				scalarOut(e)
				useIndent, lastWasKey = indent, false
			}
			level--
			addIndent(useIndent, level)
			b.WriteByte(']')
			first = false
			useIndent, lastWasKey = indent, false
		case jbObject:
			if !first {
				b.WriteString(sep)
			}
			addIndent(useIndent && !afterKey, level)
			b.WriteByte('{')
			first = true
			level++
			useIndent, lastWasKey = indent, false
			for i, k := range v.keys {
				if !first {
					b.WriteString(sep)
				}
				first = true
				addIndent(useIndent, level)
				pgEscapeJSON(&b, k)
				b.WriteString(": ")
				val := v.vals[i]
				if val.kind == jbArray || val.kind == jbObject {
					useIndent, lastWasKey = indent, true
					walk(val, true)
				} else {
					first = false
					scalarOut(val)
					useIndent, lastWasKey = indent, false
				}
			}
			level--
			addIndent(useIndent, level)
			b.WriteByte('}')
			first = false
			useIndent, lastWasKey = indent, false
		default:
			scalarOut(v)
		}
	}
	_ = lastWasKey
	walk(v, false)
	return b.String()
}

func jsonbValueOf(a any) jbValue {
	v, err := parseJSONB(string(a.(pgJSONB)))
	if err != nil {
		panic("sqlfn: stored jsonb does not parse: " + err.Error())
	}
	return v
}

func jbResult(v jbValue) any {
	if v.kind != jbArray && v.kind != jbObject {
		v.raw = true
	}
	return pgJSONB(jsonbOut(v, false))
}

// jbAsText is JsonbValueAsText: NULL for null, a string's text, a number's numeric_out, true/false,
// and a container's JSON.
func jbAsText(v jbValue) any {
	switch v.kind {
	case jbNull:
		return nil
	case jbBool:
		if v.b {
			return "true"
		}
		return "false"
	case jbString:
		return v.str
	case jbNumeric:
		return v.num.String()
	}
	return jsonbOut(v, false)
}

func jbTypeName(v jbValue) string {
	return map[jbKind]string{jbNull: "null", jbString: "string", jbNumeric: "number", jbBool: "boolean",
		jbArray: "array", jbObject: "object"}[v.kind]
}

func (v jbValue) lookup(key string) (jbValue, bool) {
	for i, k := range v.keys {
		if k == key {
			return v.vals[i], true
		}
	}
	return jbValue{}, false
}

// pathIndex is strtoint on a path element: the index, or false where it is not an integer.
func pathIndex(s string) (int, bool) {
	t := strings.TrimLeft(s, " \t\n\r\f\v")
	if t == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(t, 10, 32)
	if err != nil {
		return 0, false
	}
	return int(n), true
}

func textArrayElems(a any) ([]string, bool) {
	arr := a.(pgTextArr)
	out := make([]string, len(arr))
	for i, e := range arr {
		if e == nil {
			return nil, false
		}
		out[i] = *e
	}
	return out, true
}

func init() {
	// json_extract_path, json_extract_path_text: get_worker over the parse.
	jsonPath := func(asText bool) pgImpl {
		return func(a []any) (any, error) {
			path, ok := textArrayElems(a[1])
			if !ok {
				return nil, nil
			}
			r, err := jsonGetPath(string(a[0].(pgJSON)), path, asText)
			if err != nil || r == nil {
				return nil, err
			}
			if asText {
				return *r, nil
			}
			return pgJSON(*r), nil
		}
	}
	registerPg("json_extract_path(json,_text)", jsonPath(false))
	registerPg("json_extract_path_text(json,_text)", jsonPath(true))
	jsonbPath := func(asText bool) pgImpl {
		return func(a []any) (any, error) {
			path, ok := textArrayElems(a[1])
			if !ok {
				return nil, nil
			}
			root := jsonbValueOf(a[0])
			v := root
			if len(path) == 0 {
				if asText {
					if root.raw && root.kind == jbNull {
						return nil, nil
					}
					if root.raw {
						return jbAsText(root), nil
					}
					return jsonbOut(root, false), nil
				}
				return a[0], nil
			}
			for _, p := range path {
				switch v.kind {
				case jbObject:
					nv, ok := v.lookup(p)
					if !ok {
						return nil, nil
					}
					v = nv
				case jbArray:
					if v.raw {
						return nil, nil
					}
					idx, ok := pathIndex(p)
					if !ok {
						return nil, nil
					}
					if idx < 0 {
						if idx == math.MinInt32 || -idx > len(v.arr) {
							return nil, nil
						}
						idx += len(v.arr)
					}
					if idx >= len(v.arr) {
						return nil, nil
					}
					v = v.arr[idx]
				default:
					return nil, nil
				}
			}
			if asText {
				return jbAsText(v), nil
			}
			return jbResult(v), nil
		}
	}
	registerPg("jsonb_extract_path(jsonb,_text)", jsonbPath(false))
	registerPg("jsonb_extract_path_text(jsonb,_text)", jsonbPath(true))

	registerPg("json_array_length(json)", func(a []any) (any, error) {
		l := newPgJSONLex(string(a[0].(pgJSON)), false)
		count := 0
		err := runSem(l, &pgJSONSem{
			objectStart: func() {
				if l.level == 0 {
					raise(pgErrorf("cannot get array length of a non-array"))
				}
			},
			scalar: func(string, pgJSONToken) {
				if l.level == 0 {
					raise(pgErrorf("cannot get array length of a scalar"))
				}
			},
			arrayElementStart: func(bool) {
				if l.level == 1 {
					count++
				}
			},
		})
		if err != nil {
			return nil, err
		}
		return pgInt4(count), nil
	})
	registerPg("jsonb_array_length(jsonb)", func(a []any) (any, error) {
		v := jsonbValueOf(a[0])
		if v.raw {
			return nil, pgErrorf("cannot get array length of a scalar")
		}
		if v.kind != jbArray {
			return nil, pgErrorf("cannot get array length of a non-array")
		}
		return pgInt4(len(v.arr)), nil
	})
	registerPg("json_typeof(json)", func(a []any) (any, error) {
		l := newPgJSONLex(string(a[0].(pgJSON)), false)
		if e := l.lex(); e != jeSuccess {
			return nil, l.errorFor(e)
		}
		return map[pgJSONToken]string{jtObjectStart: "object", jtArrayStart: "array", jtString: "string",
			jtNumber: "number", jtTrue: "boolean", jtFalse: "boolean", jtNull: "null"}[l.tokenType], nil
	})
	registerPg("jsonb_typeof(jsonb)", func(a []any) (any, error) { return jbTypeName(jsonbValueOf(a[0])), nil })

	// json_each, json_each_text.
	jsonEachPg := func(asText bool) pgImpl {
		return func(a []any) (any, error) {
			l := newPgJSONLex(string(a[0].(pgJSON)), true)
			var rows pgRows
			start := 0
			next := false
			normalized := ""
			err := runSem(l, &pgJSONSem{
				arrayStart: func() {
					if l.level == 0 {
						raise(pgErrorf("cannot deconstruct an array as an object"))
					}
				},
				scalar: func(tok string, _ pgJSONToken) {
					if l.level == 0 {
						raise(pgErrorf("cannot deconstruct a scalar"))
					}
					if next {
						normalized = tok
					}
				},
				objectFieldStart: func(string, bool) {
					if l.level == 1 {
						if asText && l.tokenType == jtString {
							next = true
						} else {
							start = l.tokenStart
						}
					}
				},
				objectFieldEnd: func(fname string, isnull bool) {
					if l.level != 1 {
						return
					}
					var val any
					switch {
					case isnull && asText:
					case next:
						val = normalized
						next = false
					default:
						if asText {
							val = l.input[start:l.prevTerminator]
						} else {
							val = pgJSON(l.input[start:l.prevTerminator])
						}
					}
					rows = append(rows, []any{fname, val})
				},
			})
			if err != nil {
				return nil, err
			}
			return rows, nil
		}
	}
	registerPg("json_each(json)", jsonEachPg(false))
	registerPg("json_each_text(json)", jsonEachPg(true))
	jsonbEach := func(name string, asText bool) pgImpl {
		return func(a []any) (any, error) {
			v := jsonbValueOf(a[0])
			if v.kind != jbObject {
				return nil, pgErrorf("cannot call %s on a non-object", name)
			}
			var rows pgRows
			for i, k := range v.keys {
				if asText {
					rows = append(rows, []any{k, jbAsText(v.vals[i])})
				} else {
					rows = append(rows, []any{k, jbResult(v.vals[i])})
				}
			}
			return rows, nil
		}
	}
	registerPg("jsonb_each(jsonb)", jsonbEach("jsonb_each", false))
	registerPg("jsonb_each_text(jsonb)", jsonbEach("jsonb_each_text", true))

	// json_array_elements, json_array_elements_text.
	jsonElems := func(name string, asText bool) pgImpl {
		return func(a []any) (any, error) {
			l := newPgJSONLex(string(a[0].(pgJSON)), asText)
			var rows pgRows
			start := 0
			next := false
			normalized := ""
			err := runSem(l, &pgJSONSem{
				objectStart: func() {
					if l.level == 0 {
						raise(pgErrorf("cannot call %s on a non-array", name))
					}
				},
				scalar: func(tok string, _ pgJSONToken) {
					if l.level == 0 {
						raise(pgErrorf("cannot call %s on a scalar", name))
					}
					if next {
						normalized = tok
					}
				},
				arrayElementStart: func(bool) {
					if l.level == 1 {
						if asText && l.tokenType == jtString {
							next = true
						} else {
							start = l.tokenStart
						}
					}
				},
				arrayElementEnd: func(isnull bool) {
					if l.level != 1 {
						return
					}
					var val any
					switch {
					case isnull && asText:
					case next:
						val = normalized
						next = false
					default:
						if asText {
							val = l.input[start:l.prevTerminator]
						} else {
							val = pgJSON(l.input[start:l.prevTerminator])
						}
					}
					rows = append(rows, []any{val})
				},
			})
			if err != nil {
				return nil, err
			}
			return rows, nil
		}
	}
	registerPg("json_array_elements(json)", jsonElems("json_array_elements", false))
	registerPg("json_array_elements_text(json)", jsonElems("json_array_elements_text", true))
	jsonbElems := func(name string, asText bool) pgImpl {
		return func(a []any) (any, error) {
			v := jsonbValueOf(a[0])
			if v.raw {
				return nil, pgErrorf("cannot extract elements from a scalar")
			}
			if v.kind != jbArray {
				return nil, pgErrorf("cannot extract elements from an object")
			}
			var rows pgRows
			for _, e := range v.arr {
				if asText {
					rows = append(rows, []any{jbAsText(e)})
				} else {
					rows = append(rows, []any{jbResult(e)})
				}
			}
			return rows, nil
		}
	}
	registerPg("jsonb_array_elements(jsonb)", jsonbElems("jsonb_array_elements", false))
	registerPg("jsonb_array_elements_text(jsonb)", jsonbElems("jsonb_array_elements_text", true))

	registerPg("json_object_keys(json)", func(a []any) (any, error) {
		l := newPgJSONLex(string(a[0].(pgJSON)), true)
		var rows pgRows
		err := runSem(l, &pgJSONSem{
			arrayStart: func() {
				if l.level == 0 {
					raise(pgErrorf("cannot call json_object_keys on an array"))
				}
			},
			scalar: func(string, pgJSONToken) {
				if l.level == 0 {
					raise(pgErrorf("cannot call json_object_keys on a scalar"))
				}
			},
			objectFieldStart: func(fname string, _ bool) {
				if l.level == 1 {
					rows = append(rows, []any{fname})
				}
			},
		})
		if err != nil {
			return nil, err
		}
		return rows, nil
	})
	registerPg("jsonb_object_keys(jsonb)", func(a []any) (any, error) {
		v := jsonbValueOf(a[0])
		if v.raw {
			return nil, pgErrorf("cannot call jsonb_object_keys on a scalar")
		}
		if v.kind == jbArray {
			return nil, pgErrorf("cannot call jsonb_object_keys on an array")
		}
		var rows pgRows
		for _, k := range v.keys {
			rows = append(rows, []any{k})
		}
		return rows, nil
	})

	// Builders.
	registerPg("json_build_object()", func([]any) (any, error) { return pgJSON("{}"), nil })
	registerPg("json_build_array()", func([]any) (any, error) { return pgJSON("[]"), nil })
	registerPg("jsonb_build_object()", func([]any) (any, error) { return pgJSONB("{}"), nil })
	registerPg("jsonb_build_array()", func([]any) (any, error) { return pgJSONB("[]"), nil })
	registerPg("json_build_object(any)", func(a []any) (any, error) {
		if len(a)%2 != 0 {
			return nil, pgErrorf("argument list must have even number of elements")
		}
		var b strings.Builder
		b.WriteByte('{')
		sep := ""
		for i := 0; i < len(a); i += 2 {
			b.WriteString(sep)
			sep = ", "
			if a[i] == nil {
				return nil, pgErrorf("argument %d cannot be null", i+1)
			}
			if err := datumToJSON(&b, a[i], true); err != nil {
				return nil, err
			}
			b.WriteString(" : ")
			if err := datumToJSON(&b, a[i+1], false); err != nil {
				return nil, err
			}
		}
		b.WriteByte('}')
		return pgJSON(b.String()), nil
	})
	registerPg("json_build_array(any)", func(a []any) (any, error) {
		var b strings.Builder
		b.WriteByte('[')
		for i, v := range a {
			if i > 0 {
				b.WriteString(", ")
			}
			if err := datumToJSON(&b, v, false); err != nil {
				return nil, err
			}
		}
		b.WriteByte(']')
		return pgJSON(b.String()), nil
	})
	registerPg("to_json(anyelement)", func(a []any) (any, error) {
		var b strings.Builder
		if err := datumToJSON(&b, a[0], false); err != nil {
			return nil, err
		}
		return pgJSON(b.String()), nil
	})
	registerPg("jsonb_build_object(any)", func(a []any) (any, error) {
		if len(a)%2 != 0 {
			return nil, pgErrorf("argument list must have even number of elements")
		}
		o := jbValue{kind: jbObject}
		for i := 0; i < len(a); i += 2 {
			if a[i] == nil {
				return nil, pgErrorf("argument %d: key must not be null", i+1)
			}
			k, err := datumToJSONB(a[i], true)
			if err != nil {
				return nil, err
			}
			v, err := datumToJSONB(a[i+1], false)
			if err != nil {
				return nil, err
			}
			o.keys = append(o.keys, k.str)
			o.vals = append(o.vals, v)
		}
		return jbResult(uniqueify(o)), nil
	})
	registerPg("jsonb_build_array(any)", func(a []any) (any, error) {
		arr := jbValue{kind: jbArray}
		for _, x := range a {
			v, err := datumToJSONB(x, false)
			if err != nil {
				return nil, err
			}
			arr.arr = append(arr.arr, v)
		}
		return jbResult(arr), nil
	})
	registerPg("to_jsonb(anyelement)", func(a []any) (any, error) {
		v, err := datumToJSONB(a[0], false)
		if err != nil {
			return nil, err
		}
		return jbResult(v), nil
	})
}

// jsonGetPath is get_worker for json_extract_path(_text): the matched element's source text, a
// string de-escaped and a null as NULL when asText; nil when nothing matches.
func jsonGetPath(json string, path []string, asText bool) (*string, error) {
	l := newPgJSONLex(json, true)
	npath := len(path)
	pathok := make([]bool, npath)
	cur := make([]int, npath)
	idx := make([]int, npath)
	for i, p := range path {
		if n, ok := pathIndex(p); ok && p != "" {
			idx[i] = n
		} else {
			idx[i] = math.MinInt32
		}
	}
	if npath > 0 {
		pathok[0] = true
	}
	var result *string
	start := -1
	next := false
	set := func(s string) { result = &s }
	sem := &pgJSONSem{
		scalar: func(tok string, t pgJSONToken) {
			if l.level == 0 && npath == 0 {
				switch {
				case asText && t == jtString:
					next = true
				case asText && t == jtNull:
					result = nil
				default:
					set(l.input[:l.prevTerminator])
				}
			}
			if next {
				set(tok)
				next = false
			}
		},
		objectFieldStart: func(fname string, _ bool) {
			lv := l.level
			if lv <= npath && pathok[lv-1] && fname == path[lv-1] {
				if lv < npath {
					pathok[lv] = true
				} else {
					result = nil
					start = -1
					if asText && l.tokenType == jtString {
						next = true
					} else {
						start = l.tokenStart
					}
				}
			}
		},
		objectFieldEnd: func(fname string, isnull bool) {
			lv := l.level
			last := false
			if lv <= npath && pathok[lv-1] && fname == path[lv-1] {
				if lv < npath {
					pathok[lv] = false
				} else {
					last = true
				}
			}
			if last && start >= 0 {
				if isnull && asText {
					result = nil
				} else {
					set(l.input[start:l.prevTerminator])
				}
				start = -1
			}
		},
		arrayStart: func() {
			lv := l.level
			if lv < npath {
				cur[lv] = -1
				if idx[lv] < 0 && idx[lv] != math.MinInt32 {
					n, e := l.countArrayElements()
					if e != jeSuccess {
						raise(l.errorFor(e))
					}
					if -idx[lv] <= n {
						idx[lv] += n
					}
				}
			} else if lv == 0 && npath == 0 {
				start = l.tokenStart
			}
		},
		arrayElementStart: func(bool) {
			lv := l.level
			if lv <= npath {
				cur[lv-1]++
			}
			if lv <= npath && pathok[lv-1] && cur[lv-1] == idx[lv-1] {
				if lv < npath {
					pathok[lv] = true
				} else {
					result = nil
					start = -1
					if asText && l.tokenType == jtString {
						next = true
					} else {
						start = l.tokenStart
					}
				}
			}
		},
		arrayElementEnd: func(isnull bool) {
			lv := l.level
			last := false
			if lv <= npath && pathok[lv-1] && cur[lv-1] == idx[lv-1] {
				if lv < npath {
					pathok[lv] = false
				} else {
					last = true
				}
			}
			if last && start >= 0 {
				if isnull && asText {
					result = nil
				} else {
					set(l.input[start:l.prevTerminator])
				}
				start = -1
			}
		},
	}
	if npath == 0 {
		sem.objectStart = func() {
			if l.level == 0 {
				start = l.tokenStart
			}
		}
		sem.objectEnd = func() {
			if l.level == 0 {
				set(l.input[start:l.prevTerminator])
			}
		}
		sem.arrayEnd = func() {
			if l.level == 0 {
				set(l.input[start:l.prevTerminator])
			}
		}
	}
	if err := runSem(l, sem); err != nil {
		return nil, err
	}
	return result, nil
}

// datumToJSON is datum_to_json: a value's JSON by its type's category.
func datumToJSON(b *strings.Builder, v any, key bool) error {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		s := "false"
		if x {
			s = "true"
		}
		if key {
			pgEscapeJSON(b, s)
		} else {
			b.WriteString(s)
		}
	case pgInt4, int64, pgNumeric, pgFloat8:
		s, _ := pgTextOf(x)
		if !key && isValidJSONNumber(s) {
			b.WriteString(s)
		} else {
			pgEscapeJSON(b, s)
		}
	case pgJSON:
		if key {
			return pgErrorf("key value must be scalar, not array, composite, or json")
		}
		b.WriteString(string(x))
	case pgJSONB:
		if key {
			return pgErrorf("key value must be scalar, not array, composite, or json")
		}
		b.WriteString(string(x))
	case pgTextArr:
		if key {
			return pgErrorf("key value must be scalar, not array, composite, or json")
		}
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if e == nil {
				b.WriteString("null")
			} else {
				pgEscapeJSON(b, *e)
			}
		}
		b.WriteByte(']')
	default:
		s, _ := pgTextOf(x)
		pgEscapeJSON(b, s)
	}
	return nil
}

// isValidJSONNumber is IsValidJsonNumber.
func isValidJSONNumber(s string) bool {
	if s == "" {
		return false
	}
	t := s
	if t[0] == '-' {
		t = t[1:]
	}
	l := &pgJSONLex{input: t}
	return l.lexNumber(0) == jeSuccess && l.tokenTerminator == len(t)
}

// datumToJSONB is datum_to_jsonb.
func datumToJSONB(v any, key bool) (jbValue, error) {
	switch x := v.(type) {
	case nil:
		return jbValue{kind: jbNull}, nil
	case bool:
		if key {
			s := "false"
			if x {
				s = "true"
			}
			return jbValue{kind: jbString, str: s}, nil
		}
		return jbValue{kind: jbBool, b: x}, nil
	case pgInt4, int64, pgNumeric, pgFloat8:
		s, _ := pgTextOf(x)
		if key || strings.ContainsAny(s, "Nn") {
			return jbValue{kind: jbString, str: s}, nil
		}
		n, err := parseNumeric(s)
		if err != nil {
			return jbValue{}, err
		}
		return jbValue{kind: jbNumeric, num: n}, nil
	case pgJSON:
		if key {
			return jbValue{}, pgErrorf("key value must be scalar, not array, composite, or json")
		}
		return parseJSONB(string(x))
	case pgJSONB:
		if key {
			return jbValue{}, pgErrorf("key value must be scalar, not array, composite, or json")
		}
		return jsonbValueOf(x), nil
	case pgTextArr:
		if key {
			return jbValue{}, pgErrorf("key value must be scalar, not array, composite, or json")
		}
		arr := jbValue{kind: jbArray}
		for _, e := range x {
			if e == nil {
				arr.arr = append(arr.arr, jbValue{kind: jbNull})
			} else {
				arr.arr = append(arr.arr, jbValue{kind: jbString, str: *e})
			}
		}
		return arr, nil
	}
	s, _ := pgTextOf(v)
	return jbValue{kind: jbString, str: s}, nil
}
