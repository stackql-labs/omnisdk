package sqlfn

import (
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

func builtins() []Func {
	fns := []Func{
		// Text.
		NewScalar("lower", 1, 1, strict1(func(a []any) (any, error) { return strings.ToLower(text(a[0])), nil })),
		NewScalar("upper", 1, 1, strict1(func(a []any) (any, error) { return strings.ToUpper(text(a[0])), nil })),
		NewScalar("length", 1, 1, strict1(func(a []any) (any, error) { return int64(utf8.RuneCountInString(text(a[0]))), nil })),
		NewScalar("substr", 2, 3, strict(substr)),
		NewScalar("substring", 2, 3, strict(substr)),
		NewScalar("instr", 2, 2, strict(func(a []any) (any, error) {
			i := strings.Index(text(a[0]), text(a[1]))
			if i < 0 {
				return int64(0), nil
			}
			return int64(utf8.RuneCountInString(text(a[0])[:i]) + 1), nil
		})),
		NewScalar("replace", 3, 3, strict(func(a []any) (any, error) {
			return strings.ReplaceAll(text(a[0]), text(a[1]), text(a[2])), nil
		})),
		NewScalar("trim", 1, 2, strict(trimWith(strings.Trim, strings.TrimSpace))),
		NewScalar("ltrim", 1, 2, strict(trimWith(strings.TrimLeft, func(s string) string { return strings.TrimLeft(s, " ") }))),
		NewScalar("rtrim", 1, 2, strict(trimWith(strings.TrimRight, func(s string) string { return strings.TrimRight(s, " ") }))),
		NewScalar("concat", 1, -1, func(a []any) (any, error) {
			var b strings.Builder
			for _, v := range a {
				if v != nil {
					b.WriteString(text(v))
				}
			}
			return b.String(), nil
		}),
		NewScalar("regexp_replace", 3, 4, strict(regexpReplace)),
		NewScalar("regexp_substr", 2, 2, strict(func(a []any) (any, error) {
			re, err := regexp.Compile(text(a[1]))
			if err != nil {
				return nil, err
			}
			if m := re.FindString(text(a[0])); m != "" || re.MatchString(text(a[0])) {
				return m, nil
			}
			return nil, nil
		})),
		NewScalar("regexp_like", 2, 3, strict(func(a []any) (any, error) {
			pattern := text(a[1])
			if len(a) == 3 && strings.Contains(text(a[2]), "i") {
				pattern = "(?i)" + pattern
			}
			re, err := regexp.Compile(pattern)
			if err != nil {
				return nil, err
			}
			return re.MatchString(text(a[0])), nil
		})),
		// Null handling.
		NewScalar("coalesce", 1, -1, func(a []any) (any, error) {
			for _, v := range a {
				if v != nil {
					return v, nil
				}
			}
			return nil, nil
		}),
		NewScalar("ifnull", 2, 2, func(a []any) (any, error) {
			if a[0] != nil {
				return a[0], nil
			}
			return a[1], nil
		}),
		NewScalar("nullif", 2, 2, func(a []any) (any, error) {
			if a[0] != nil && a[1] != nil && text(a[0]) == text(a[1]) {
				return nil, nil
			}
			return a[0], nil
		}),
		NewScalar("typeof", 1, 1, func(a []any) (any, error) { return typeOf(a[0]), nil }),
		// Numbers.
		NewScalar("abs", 1, 1, numeric(math.Abs)),
		NewScalar("floor", 1, 1, numeric(math.Floor)),
		NewScalar("ceil", 1, 1, numeric(math.Ceil)),
		NewScalar("ceiling", 1, 1, numeric(math.Ceil)),
		NewScalar("round", 1, 2, strict(func(a []any) (any, error) {
			x, err := number(a[0])
			if err != nil {
				return nil, err
			}
			places := 0.0
			if len(a) == 2 {
				if places, err = number(a[1]); err != nil {
					return nil, err
				}
			}
			p := math.Pow(10, places)
			return math.Round(x*p) / p, nil
		})),
		// JSON.
		NewScalar("json", 1, 1, strict1(func(a []any) (any, error) {
			v, err := decodeJSON(a[0])
			if err != nil {
				return nil, err
			}
			return encodeJSON(v)
		})),
		NewScalar("json_extract", 2, -1, strict(jsonExtract)),
		NewScalar("json_extract_path_text", 1, -1, strict(jsonExtractPathText)),
		NewScalar("json_array_length", 1, 2, strict(func(a []any) (any, error) {
			v, err := jsonAt(a)
			if err != nil || v == nil {
				return nil, err
			}
			arr, ok := v.([]any)
			if !ok {
				return int64(0), nil
			}
			return int64(len(arr)), nil
		})),
		NewScalar("json_type", 1, 2, strict(func(a []any) (any, error) {
			v, err := jsonAt(a)
			if err != nil {
				return nil, err
			}
			return jsonTypeName(v), nil
		})),
		NewScalar("json_object", 0, -1, jsonObject),
		NewScalar("json_build_object", 0, -1, jsonObject),
		NewScalar("json_array", 0, -1, func(a []any) (any, error) {
			arr := make([]any, len(a))
			for i, v := range a {
				arr[i] = jsonValue(v)
			}
			return encodeJSON(arr)
		}),
		NewScalar("json_equal", 2, 2, strict(func(a []any) (any, error) {
			x, err := decodeJSON(a[0])
			if err != nil {
				return nil, err
			}
			y, err := decodeJSON(a[1])
			if err != nil {
				return nil, err
			}
			return reflect.DeepEqual(x, y), nil
		})),
		NewScalar("aws_policy_equal", 2, 2, strict(func(a []any) (any, error) {
			x, err := awsPolicy(a[0])
			if err != nil {
				return nil, err
			}
			y, err := awsPolicy(a[1])
			if err != nil {
				return nil, err
			}
			return reflect.DeepEqual(x, y), nil
		})),
		// Dates.
		NewScalar("date", 1, -1, dateFn(func(t timeValue) any { return t.Format("2006-01-02") })),
		NewScalar("datetime", 1, -1, dateFn(func(t timeValue) any { return t.Format("2006-01-02 15:04:05") })),
		NewScalar("julianday", 1, -1, dateFn(func(t timeValue) any { return julian(t) })),
		NewScalar("unixepoch", 1, -1, dateFn(func(t timeValue) any { return t.Unix() })),
		// Tables.
		NewTable("json_each", []string{"key", "value", "type"}, 1, 2, jsonEach),
		NewTable("json_array_elements_text", []string{"value"}, 1, 1, func(a []any) ([]map[string]any, error) {
			return elements(a[0], func(v any) any {
				if v == nil {
					return nil
				}
				if s, ok := v.(string); ok {
					return s
				}
				s, _ := encodeJSON(v)
				return s
			})
		}),
		NewTable("unnest", []string{"value"}, 1, 1, func(a []any) ([]map[string]any, error) {
			return elements(a[0], func(v any) any { return sqlValue(v) })
		}),
		NewTable("generate_subscripts", []string{"value"}, 1, 2, func(a []any) ([]map[string]any, error) {
			if a[0] == nil {
				return nil, nil
			}
			v, err := decodeJSON(a[0])
			if err != nil {
				return nil, err
			}
			arr, _ := v.([]any)
			rows := make([]map[string]any, len(arr))
			for i := range arr {
				rows[i] = map[string]any{"value": int64(i + 1)}
			}
			return rows, nil
		}),
	}
	return fns
}

// strict makes a function NULL-propagating: any NULL argument is a NULL result.
func strict(f func([]any) (any, error)) func([]any) (any, error) {
	return func(a []any) (any, error) {
		for _, v := range a {
			if v == nil {
				return nil, nil
			}
		}
		return f(a)
	}
}

func strict1(f func([]any) (any, error)) func([]any) (any, error) { return strict(f) }

// text is a value's written form: a string as is, a number in its shortest form, a container as JSON.
func text(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "1"
		}
		return "0"
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(t), 'f', -1, 32)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case json.Number:
		return t.String()
	case map[string]any, []any:
		s, _ := encodeJSON(t)
		return s.(string)
	}
	return fmt.Sprint(v)
}

func number(v any) (float64, error) {
	switch t := v.(type) {
	case float64:
		return t, nil
	case int:
		return float64(t), nil
	case int64:
		return float64(t), nil
	case bool:
		if t {
			return 1, nil
		}
		return 0, nil
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(text(v)), 64)
	if err != nil {
		return 0, fmt.Errorf("sqlfn: %q is not a number", text(v))
	}
	return f, nil
}

func numeric(f func(float64) float64) func([]any) (any, error) {
	return strict(func(a []any) (any, error) {
		x, err := number(a[0])
		if err != nil {
			return nil, err
		}
		return f(x), nil
	})
}

// substr is SQLite's: 1-based, a negative start counts from the end, an optional length.
func substr(a []any) (any, error) {
	r := []rune(text(a[0]))
	start, err := number(a[1])
	if err != nil {
		return nil, err
	}
	s := int(start)
	if s < 0 {
		s = len(r) + s + 1
	}
	if s < 1 {
		s = 1
	}
	end := len(r)
	if len(a) == 3 {
		n, err := number(a[2])
		if err != nil {
			return nil, err
		}
		if n < 0 {
			return "", nil
		}
		end = min(len(r), s-1+int(n))
	}
	if s-1 >= end {
		return "", nil
	}
	return string(r[s-1 : end]), nil
}

func trimWith(cut func(string, string) string, space func(string) string) func([]any) (any, error) {
	return func(a []any) (any, error) {
		if len(a) == 2 {
			return cut(text(a[0]), text(a[1])), nil
		}
		return space(text(a[0])), nil
	}
}

// regexpReplace is PostgreSQL's: the first match only, unless flags carry "g"; "i" ignores case.
// Back-references are written \1 in the replacement.
func regexpReplace(a []any) (any, error) {
	pattern, flags := text(a[1]), ""
	if len(a) == 4 {
		flags = text(a[3])
	}
	if strings.Contains(flags, "i") {
		pattern = "(?i)" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}
	repl := regexp.MustCompile(`\\(\d)`).ReplaceAllString(text(a[2]), "$${$1}")
	s := text(a[0])
	if strings.Contains(flags, "g") {
		return re.ReplaceAllString(s, repl), nil
	}
	done := false
	return re.ReplaceAllStringFunc(s, func(m string) string {
		if done {
			return m
		}
		done = true
		return re.ReplaceAllString(m, repl)
	}), nil
}

func typeOf(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case bool, int, int64:
		return "integer"
	case float64:
		if t == math.Trunc(t) && math.Abs(t) < 1<<53 {
			return "integer"
		}
		return "real"
	case json.Number:
		if _, err := t.Int64(); err == nil {
			return "integer"
		}
		return "real"
	}
	return "text"
}

// decodeJSON reads a JSON value: a string is parsed as JSON text, a container is taken as is.
func decodeJSON(v any) (any, error) {
	s, ok := v.(string)
	if !ok {
		return v, nil
	}
	var out any
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("sqlfn: malformed JSON: %w", err)
	}
	return out, nil
}

func encodeJSON(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

// sqlValue is a JSON value as SQLite returns it: a boolean as 1 or 0, a container as JSON text.
func sqlValue(v any) any {
	switch t := v.(type) {
	case bool:
		if t {
			return int64(1)
		}
		return int64(0)
	case map[string]any, []any:
		s, _ := encodeJSON(t)
		return s
	}
	return v
}

// jsonValue is an argument as a JSON value: text that is a JSON container stays structured.
func jsonValue(v any) any {
	if s, ok := v.(string); ok && (strings.HasPrefix(s, "{") || strings.HasPrefix(s, "[")) {
		var out any
		if json.Unmarshal([]byte(s), &out) == nil {
			return out
		}
	}
	return v
}

func jsonObject(a []any) (any, error) {
	if len(a)%2 != 0 {
		return nil, fmt.Errorf("sqlfn: json_object takes key, value pairs")
	}
	obj := make(map[string]any, len(a)/2)
	for i := 0; i < len(a); i += 2 {
		obj[text(a[i])] = jsonValue(a[i+1])
	}
	return encodeJSON(obj)
}

func jsonTypeName(v any) any {
	switch t := v.(type) {
	case nil:
		return "null"
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		if t == math.Trunc(t) {
			return "integer"
		}
		return "real"
	case string:
		return "text"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return nil
}

// jsonAt is the value at an optional path argument: json_x(json [, path]).
func jsonAt(a []any) (any, error) {
	v, err := decodeJSON(a[0])
	if err != nil {
		return nil, err
	}
	if len(a) < 2 {
		return v, nil
	}
	got, _, err := walkPath(v, text(a[1]))
	return got, err
}

// jsonExtract is SQLite's json_extract: one path gives its value, several give a JSON array of them.
func jsonExtract(a []any) (any, error) {
	v, err := decodeJSON(a[0])
	if err != nil {
		return nil, err
	}
	if len(a) == 2 {
		got, found, err := walkPath(v, text(a[1]))
		if err != nil || !found {
			return nil, err
		}
		return sqlValue(got), nil
	}
	out := make([]any, 0, len(a)-1)
	for _, p := range a[1:] {
		got, _, err := walkPath(v, text(p))
		if err != nil {
			return nil, err
		}
		out = append(out, got)
	}
	return encodeJSON(out)
}

// jsonExtractPathText is PostgreSQL's: follow keys (array positions as numbers), and return text.
func jsonExtractPathText(a []any) (any, error) {
	v, err := decodeJSON(a[0])
	if err != nil {
		return nil, err
	}
	for _, k := range a[1:] {
		switch t := v.(type) {
		case map[string]any:
			v = t[text(k)]
		case []any:
			i, err := strconv.Atoi(text(k))
			if err != nil || i < 0 || i >= len(t) {
				return nil, nil
			}
			v = t[i]
		default:
			return nil, nil
		}
	}
	switch t := v.(type) {
	case nil:
		return nil, nil
	case string:
		return t, nil
	case bool:
		return strconv.FormatBool(t), nil
	}
	return text(v), nil
}

// walkPath follows a SQLite JSON path: $, .key, ."quoted key", [n], [#-n].
func walkPath(v any, path string) (any, bool, error) {
	if !strings.HasPrefix(path, "$") {
		return nil, false, fmt.Errorf("sqlfn: JSON path %q must start with $", path)
	}
	rest := path[1:]
	for rest != "" {
		switch rest[0] {
		case '.':
			rest = rest[1:]
			var key string
			if strings.HasPrefix(rest, `"`) {
				end := strings.Index(rest[1:], `"`)
				if end < 0 {
					return nil, false, fmt.Errorf("sqlfn: JSON path %q has an unclosed quote", path)
				}
				key, rest = rest[1:end+1], rest[end+2:]
			} else {
				end := strings.IndexAny(rest, ".[")
				if end < 0 {
					end = len(rest)
				}
				key, rest = rest[:end], rest[end:]
			}
			obj, ok := v.(map[string]any)
			if !ok {
				return nil, false, nil
			}
			if v, ok = obj[key]; !ok {
				return nil, false, nil
			}
		case '[':
			end := strings.Index(rest, "]")
			if end < 0 {
				return nil, false, fmt.Errorf("sqlfn: JSON path %q has an unclosed [", path)
			}
			idx := rest[1:end]
			rest = rest[end+1:]
			arr, ok := v.([]any)
			if !ok {
				return nil, false, nil
			}
			var i int
			var err error
			if strings.HasPrefix(idx, "#") {
				var back int
				if idx != "#" {
					back, err = strconv.Atoi(strings.TrimPrefix(idx[1:], "-"))
					back = -back
				}
				i = len(arr) + back
			} else {
				i, err = strconv.Atoi(idx)
			}
			if err != nil {
				return nil, false, fmt.Errorf("sqlfn: JSON path %q has a bad index %q", path, idx)
			}
			if i < 0 || i >= len(arr) {
				return nil, false, nil
			}
			v = arr[i]
		default:
			return nil, false, fmt.Errorf("sqlfn: JSON path %q is malformed at %q", path, rest)
		}
	}
	return v, true, nil
}

// jsonEach is SQLite's json_each: one row per member of an object or element of an array at the
// optional path; a scalar is one row of itself.
func jsonEach(a []any) ([]map[string]any, error) {
	if a[0] == nil {
		return nil, nil
	}
	v, err := jsonAt(a)
	if err != nil {
		return nil, err
	}
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		rows := make([]map[string]any, 0, len(t))
		for _, k := range keys {
			rows = append(rows, map[string]any{"key": k, "value": sqlValue(t[k]), "type": jsonTypeName(t[k])})
		}
		return rows, nil
	case []any:
		rows := make([]map[string]any, 0, len(t))
		for i, e := range t {
			rows = append(rows, map[string]any{"key": int64(i), "value": sqlValue(e), "type": jsonTypeName(e)})
		}
		return rows, nil
	case nil:
		return nil, nil
	}
	return []map[string]any{{"key": nil, "value": sqlValue(v), "type": jsonTypeName(v)}}, nil
}

// elements is one "value" row per element of a JSON array.
func elements(arg any, render func(any) any) ([]map[string]any, error) {
	if arg == nil {
		return nil, nil
	}
	v, err := decodeJSON(arg)
	if err != nil {
		return nil, err
	}
	arr, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("sqlfn: not a JSON array")
	}
	rows := make([]map[string]any, len(arr))
	for i, e := range arr {
		rows[i] = map[string]any{"value": render(e)}
	}
	return rows, nil
}

// awsPolicy normalises an IAM policy document for comparison: URL-decoded where AWS returns it so, a
// lone statement, action or resource treated as a list of one, and list order ignored.
func awsPolicy(v any) (any, error) {
	if s, ok := v.(string); ok && strings.Contains(s, "%") {
		if dec, err := url.QueryUnescape(s); err == nil {
			v = dec
		}
	}
	doc, err := decodeJSON(v)
	if err != nil {
		return nil, err
	}
	return canonical(doc, ""), nil
}

// listKeys are the policy grammar's elements that take a value or a list of values.
var listKeys = map[string]bool{"Statement": true, "Action": true, "NotAction": true, "Resource": true,
	"NotResource": true, "AWS": true, "Service": true, "Federated": true, "CanonicalUser": true}

func canonical(v any, key string) any {
	if _, isList := v.([]any); listKeys[key] && !isList {
		v = []any{v}
	}
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = canonical(x, k)
		}
		return out
	case []any:
		items := make([]any, len(t))
		for i, x := range t {
			items[i] = canonical(x, "")
		}
		sort.Slice(items, func(i, j int) bool { return text(items[i]) < text(items[j]) })
		return items
	}
	return v
}
