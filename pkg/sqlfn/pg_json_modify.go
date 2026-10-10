package sqlfn

import (
	"math"
	"strconv"
	"strings"
)

// The Postgres JSON functions that build or rewrite a document: json_object (json.c),
// json_strip_nulls, jsonb_strip_nulls, jsonb_set and jsonb_insert (jsonfuncs.c), and jsonb_pretty.

// setPath's operations (jsonfuncs.c's JB_PATH_*).
const (
	jbPathCreate       = 0x0001
	jbPathReplace      = 0x0004
	jbPathInsertBefore = 0x0008
	jbPathInsertAfter  = 0x0010
	jbPathCreateOrIns  = jbPathInsertBefore | jbPathInsertAfter | jbPathCreate
)

// jbSetter is setPath's walk of a document, rebuilding it with newval at path.
type jbSetter struct {
	path   []*string
	newval jbValue
	op     int
}

func (s *jbSetter) last(level int) bool { return level == len(s.path)-1 }

func (s *jbSetter) set(v jbValue, level int) (jbValue, error) {
	if s.path[level] == nil {
		return v, pgErrorf("path element at position %d is null", level+1)
	}
	switch v.kind {
	case jbArray:
		return s.setArray(v, level)
	case jbObject:
		return s.setObject(v, level)
	}
	return v, nil
}

func (s *jbSetter) setObject(o jbValue, level int) (jbValue, error) {
	key := *s.path[level]
	out := jbValue{kind: jbObject}
	add := func(k string, v jbValue) {
		out.keys = append(out.keys, k)
		out.vals = append(out.vals, v)
	}
	n := len(o.keys)
	if n == 0 && s.op&jbPathCreateOrIns != 0 && s.last(level) {
		add(key, s.newval)
	}
	done := false
	for i := 0; i < n; i++ {
		k, v := o.keys[i], o.vals[i]
		if !done && k == key {
			done = true
			if s.last(level) {
				if s.op&(jbPathInsertBefore|jbPathInsertAfter) != 0 {
					return o, pgErrorf("cannot replace existing key")
				}
				add(k, s.newval)
				continue
			}
			nv, err := s.set(v, level+1)
			if err != nil {
				return o, err
			}
			add(k, nv)
			continue
		}
		if s.op&jbPathCreateOrIns != 0 && !done && s.last(level) && i == n-1 {
			add(key, s.newval)
		}
		add(k, v)
	}
	return uniqueify(out), nil
}

func (s *jbSetter) setArray(a jbValue, level int) (jbValue, error) {
	n := int32(len(a.arr))
	c := *s.path[level]
	idx, ok := strToInt32(c)
	if !ok {
		return a, pgErrorf("path element at position %d is not an integer: \"%s\"", level+1, c)
	}
	if idx < 0 {
		if uint32(-idx) > uint32(n) { // as C compares them: nelems is unsigned, so -INT_MIN is 2^31
			idx = math.MinInt32
		} else {
			idx = n + idx
		}
	}
	if idx > 0 && idx > n {
		idx = n
	}
	out := jbValue{kind: jbArray, arr: []jbValue{}}
	done := false
	if (idx == math.MinInt32 || n == 0) && s.last(level) && s.op&jbPathCreateOrIns != 0 {
		out.arr = append(out.arr, s.newval)
		done = true
	}
	for i := int32(0); i < n; i++ {
		e := a.arr[i]
		if i != idx {
			out.arr = append(out.arr, e)
			continue
		}
		done = true
		if !s.last(level) {
			ne, err := s.set(e, level+1)
			if err != nil {
				return a, err
			}
			out.arr = append(out.arr, ne)
			continue
		}
		if s.op&(jbPathInsertBefore|jbPathCreate) != 0 {
			out.arr = append(out.arr, s.newval)
		}
		if s.op&(jbPathInsertAfter|jbPathInsertBefore) != 0 {
			out.arr = append(out.arr, e)
		}
		if s.op&(jbPathInsertAfter|jbPathReplace) != 0 {
			out.arr = append(out.arr, s.newval)
		}
	}
	if s.op&jbPathCreateOrIns != 0 && !done && s.last(level) {
		out.arr = append(out.arr, s.newval)
	}
	return out, nil
}

// strToInt32 is strtoint in base 10: leading space and a sign allowed, nothing after the digits.
func strToInt32(s string) (int32, bool) {
	t := strings.TrimLeft(s, " \t\n\v\f\r")
	digits := strings.TrimLeft(t, "+-")
	if len(t)-len(digits) > 1 || digits == "" || strings.IndexFunc(digits, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(t, 10, 32)
	if err != nil {
		return 0, false
	}
	return int32(n), true
}

// jbSet is jsonb_set and jsonb_insert.
func jbSet(a []any, op int, emptyUnchanged bool) (any, error) {
	in := jsonbValueOf(a[0])
	if in.raw {
		return nil, pgErrorf("cannot set path in scalar")
	}
	count := len(in.arr) + len(in.keys)
	if emptyUnchanged && count == 0 {
		return jbResult(in), nil
	}
	path := a[1].(pgTextArr)
	if len(path) == 0 {
		return jbResult(in), nil
	}
	nv := jsonbValueOf(a[2])
	nv.raw = false
	s := &jbSetter{path: path, newval: nv, op: op}
	out, err := s.set(in, 0)
	if err != nil {
		return nil, err
	}
	return jbResult(out), nil
}

func jbStripNulls(v jbValue) jbValue {
	switch v.kind {
	case jbArray:
		out := jbValue{kind: jbArray, arr: make([]jbValue, len(v.arr))}
		for i, e := range v.arr {
			out.arr[i] = jbStripNulls(e)
		}
		return out
	case jbObject:
		out := jbValue{kind: jbObject}
		for i, k := range v.keys {
			if v.vals[i].kind == jbNull {
				continue
			}
			out.keys = append(out.keys, k)
			out.vals = append(out.vals, jbStripNulls(v.vals[i]))
		}
		return out
	}
	return v
}

// jsonObjectText is json_object's output of key/value pairs.
func jsonObjectText(keys, vals []*string) (any, error) {
	var b strings.Builder
	b.WriteByte('{')
	for i := range keys {
		if keys[i] == nil {
			return nil, pgErrorf("null value not allowed for object key")
		}
		if i > 0 {
			b.WriteString(", ")
		}
		pgEscapeJSON(&b, *keys[i])
		b.WriteString(" : ")
		if vals[i] == nil {
			b.WriteString("null")
		} else {
			pgEscapeJSON(&b, *vals[i])
		}
	}
	b.WriteByte('}')
	return pgJSON(b.String()), nil
}

func init() {
	registerPg("jsonb_set(jsonb,_text,jsonb,bool)", func(a []any) (any, error) {
		op := jbPathReplace
		if a[3].(bool) {
			op = jbPathCreate
		}
		return jbSet(a, op, op != jbPathCreate)
	})
	registerPg("jsonb_insert(jsonb,_text,jsonb,bool)", func(a []any) (any, error) {
		op := jbPathInsertBefore
		if a[3].(bool) {
			op = jbPathInsertAfter
		}
		return jbSet(a, op, false)
	})
	registerPg("jsonb_pretty(jsonb)", func(a []any) (any, error) { return jsonbOut(jsonbValueOf(a[0]), true), nil })
	registerPg("jsonb_strip_nulls(jsonb)", func(a []any) (any, error) {
		v := jsonbValueOf(a[0])
		if v.raw {
			return jbResult(v), nil
		}
		return jbResult(jbStripNulls(v)), nil
	})
	registerPg("json_strip_nulls(json)", func(a []any) (any, error) {
		l := newPgJSONLex(string(a[0].(pgJSON)), true)
		var b strings.Builder
		skipNull := false
		lastIs := func(c byte) bool { s := b.String(); return s[len(s)-1] == c }
		err := runSem(l, &pgJSONSem{
			objectStart: func() { b.WriteByte('{') },
			objectEnd:   func() { b.WriteByte('}') },
			arrayStart:  func() { b.WriteByte('[') },
			arrayEnd:    func() { b.WriteByte(']') },
			objectFieldStart: func(fname string, isnull bool) {
				if isnull {
					skipNull = true
					return
				}
				if !lastIs('{') {
					b.WriteByte(',')
				}
				pgEscapeJSON(&b, fname)
				b.WriteByte(':')
			},
			arrayElementStart: func(bool) {
				if !lastIs('[') {
					b.WriteByte(',')
				}
			},
			scalar: func(tok string, t pgJSONToken) {
				if skipNull {
					skipNull = false
					return
				}
				if t == jtString {
					pgEscapeJSON(&b, tok)
				} else {
					b.WriteString(tok)
				}
			},
		})
		if err != nil {
			return nil, err
		}
		return pgJSON(b.String()), nil
	})
	registerPg("json_object(_text)", func(a []any) (any, error) {
		arr := a[0].(pgTextArr)
		if len(arr) == 0 {
			return pgJSON("{}"), nil
		}
		if len(arr)%2 != 0 {
			return nil, pgErrorf("array must have even number of elements")
		}
		var keys, vals []*string
		for i := 0; i < len(arr); i += 2 {
			keys = append(keys, arr[i])
			vals = append(vals, arr[i+1])
		}
		return jsonObjectText(keys, vals)
	})
	registerPg("json_object(_text,_text)", func(a []any) (any, error) {
		keys, vals := a[0].(pgTextArr), a[1].(pgTextArr)
		if (len(keys) == 0) != (len(vals) == 0) {
			return nil, pgErrorf("wrong number of array subscripts")
		}
		if len(keys) == 0 {
			return pgJSON("{}"), nil
		}
		if len(keys) != len(vals) {
			return nil, pgErrorf("mismatched array dimensions")
		}
		return jsonObjectText(keys, vals)
	})
}
