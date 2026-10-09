package sqlfn

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// stackql's own SQLite functions, ported from any-sdk public/sqlfuncs (v0.6.0-alpha01, the version
// stackql pins), which registers them on stackql's embedded SQLite: split_part, regexp_like, regexp
// (the REGEXP operator), regexp_substr, regexp_replace, json_equal and aws_policy_equal. any-sdk's
// golden vectors in testdata/anysdk pin them. Arguments are first mapped onto SQLite's storage classes,
// as they would be on arrival in SQLite, then coerced as any-sdk coerces a driver value.

func stackqlExtensions() []Func {
	return []Func{
		NewScalar("split_part", 3, 3, func(a []any) (any, error) { return extSplitPart(a[0], a[1], a[2]) }),
		NewScalar("regexp_like", 2, 2, func(a []any) (any, error) { return extRegexpLike(a[0], a[1]) }),
		NewScalar("regexp", 2, 2, func(a []any) (any, error) { return extRegexpLike(a[1], a[0]) }),
		NewScalar("regexp_substr", 2, 2, func(a []any) (any, error) { return extRegexpSubstr(a[0], a[1]) }),
		NewScalar("regexp_replace", 3, 3, func(a []any) (any, error) { return extRegexpReplace(a[0], a[1], a[2]) }),
		NewScalar("json_equal", 2, 2, func(a []any) (any, error) { return extJSONEqual(a[0], a[1]) }),
		NewScalar("aws_policy_equal", 2, 2, func(a []any) (any, error) { return extAWSPolicyEqual(a[0], a[1]) }),
	}
}

// extValue is v as the SQLite driver hands it to a Go function: NULL, int64, float64, string or
// []byte.
func extValue(v any) any {
	_, x := norm(v)
	return x
}

// extText is any-sdk's valueText: false for NULL; a REAL in its shortest form with a point or
// exponent.
func extText(v any) (string, bool) {
	switch t := extValue(v).(type) {
	case nil:
		return "", false
	case string:
		return t, true
	case []byte:
		return string(t), true
	case int64:
		return strconv.FormatInt(t, 10), true
	case float64:
		s := strconv.FormatFloat(t, 'g', -1, 64)
		if !strings.ContainsAny(s, ".eE") && !math.IsInf(t, 0) && !math.IsNaN(t) {
			s += ".0"
		}
		return s, true
	default:
		return fmt.Sprintf("%v", t), true
	}
}

// extInt is any-sdk's valueInt: NULL 0, a REAL truncated, text by its signed leading digits.
func extInt(v any) int64 {
	switch t := extValue(v).(type) {
	case int64:
		return t
	case float64:
		if math.IsNaN(t) {
			return 0
		}
		return int64(math.Trunc(t))
	case string:
		return extIntPrefix(t)
	case []byte:
		return extIntPrefix(string(t))
	}
	return 0
}

func extIntPrefix(s string) int64 {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r' || s[i] == '\v' || s[i] == '\f') {
		i++
	}
	start := i
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		i++
	}
	digits := i
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == digits {
		return 0
	}
	n, err := strconv.ParseInt(s[start:i], 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// extSplitPart is split_part(source, separator, part): the 1-based part, negative from the end; NULL
// for a NULL source, a NULL or empty separator, or a part out of range.
func extSplitPart(source, separator, part any) (any, error) {
	src, srcOK := extText(source)
	sep, sepOK := extText(separator)
	if !srcOK || !sepOK || sep == "" {
		return nil, nil
	}
	idx := extInt(part)
	parts := strings.Split(src, sep)
	n := int64(len(parts))
	if idx < 0 {
		idx = n + idx
	} else {
		idx--
	}
	if idx >= 0 && idx < n {
		return parts[idx], nil
	}
	return nil, nil
}

// extPattern compiles pattern with '.' matching newline, as the C engine stackql first shipped did.
func extPattern(pattern string) (*regexp.Regexp, error) {
	re, err := regexp.Compile("(?s)" + pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid regular expression: %w", err)
	}
	return re, nil
}

func extRegexpLike(source, pattern any) (any, error) {
	src, srcOK := extText(source)
	pat, patOK := extText(pattern)
	if !srcOK || !patOK {
		return nil, nil
	}
	re, err := extPattern(pat)
	if err != nil {
		return nil, err
	}
	if re.MatchString(src) {
		return int64(1), nil
	}
	return int64(0), nil
}

func extRegexpSubstr(source, pattern any) (any, error) {
	src, srcOK := extText(source)
	pat, patOK := extText(pattern)
	if !srcOK || !patOK {
		return nil, nil
	}
	re, err := extPattern(pat)
	if err != nil {
		return nil, err
	}
	loc := re.FindStringIndex(src)
	if loc == nil {
		return nil, nil
	}
	return src[loc[0]:loc[1]], nil
}

// extRegexpReplace replaces every match with the replacement taken literally: $1 is not expanded.
func extRegexpReplace(source, pattern, replacement any) (any, error) {
	src, srcOK := extText(source)
	pat, patOK := extText(pattern)
	rep, repOK := extText(replacement)
	if !srcOK || !patOK || !repOK {
		return nil, nil
	}
	re, err := extPattern(pat)
	if err != nil {
		return nil, err
	}
	return re.ReplaceAllLiteralString(src, rep), nil
}

// Error texts of the C implementations stackql first shipped.
var (
	errInvalidJSONArgs   = errors.New("Invalid JSON strings")             //nolint:staticcheck // C-parity text
	errJSONParse         = errors.New("Error parsing JSON strings")       //nolint:staticcheck // C-parity text
	errInvalidPolicyArgs = errors.New("Invalid policy strings")           //nolint:staticcheck // C-parity text
	errPolicyParse       = errors.New("Error parsing policy JSON strings") //nolint:staticcheck // C-parity text
)

const dblEpsilon = 2.220446049250313e-16

func extCompareDouble(a, b float64) bool {
	m := math.Abs(a)
	if x := math.Abs(b); x > m {
		m = x
	}
	return math.Abs(a-b) <= m*dblEpsilon
}

func extFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return asciiCase(a, false) == asciiCase(b, false)
}

// extParseJSON parses exactly one JSON value.
func extParseJSON(s string) (any, error) {
	dec := json.NewDecoder(strings.NewReader(s))
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("trailing content after JSON value")
	}
	return v, nil
}

// extJSONEqual is json_equal(a, b): objects unordered, arrays ordered, numbers equal within epsilon.
func extJSONEqual(a, b any) (any, error) {
	sa, okA := extText(a)
	sb, okB := extText(b)
	if !okA || !okB {
		return nil, errInvalidJSONArgs
	}
	va, errA := extParseJSON(sa)
	vb, errB := extParseJSON(sb)
	if errA != nil || errB != nil {
		return nil, errJSONParse
	}
	return boolInt(extDeepEqual(va, vb)), nil
}

func extDeepEqual(a, b any) bool {
	switch av := a.(type) {
	case nil:
		return b == nil
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	case float64:
		bv, ok := b.(float64)
		return ok && extCompareDouble(av, bv)
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !extDeepEqual(av[i], bv[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, v := range av {
			w, present := bv[k]
			if !present || !extDeepEqual(v, w) {
				return false
			}
		}
		return true
	}
	return false
}

// awsUnorderedFields are the IAM policy fields whose arrays compare as sets and whose arn: strings
// compare case-insensitively.
var awsUnorderedFields = map[string]bool{
	"Action": true, "NotAction": true, "Resource": true, "NotResource": true, "Principal": true,
	"NotPrincipal": true, "AWS": true, "Service": true, "Tags": true, "tags": true,
}

// extAWSPolicyEqual is aws_policy_equal(a, b): IAM policy equivalence by the rules stackql's C
// implementation applied.
func extAWSPolicyEqual(a, b any) (any, error) {
	sa, okA := extText(a)
	sb, okB := extText(b)
	if !okA || !okB {
		return nil, errInvalidPolicyArgs
	}
	if sa == sb {
		return int64(1), nil
	}
	va, errA := extParseJSON(sa)
	vb, errB := extParseJSON(sb)
	if errA != nil || errB != nil {
		return nil, errPolicyParse
	}
	_, aArr := va.([]any)
	_, bArr := vb.([]any)
	return boolInt(awsPolicyCompare(va, vb, aArr && bArr)), nil
}

// awsPolicyCompare: a one-element array equals its lone string, unordered fields compare as sets, keys
// fold case, and the arn: case-fold tests the left operand only.
func awsPolicyCompare(a, b any, unordered bool) bool {
	if aArr, ok := a.([]any); ok {
		if bStr, isStr := b.(string); isStr {
			return len(aArr) == 1 && awsPolicyCompare(aArr[0], bStr, unordered)
		}
	}
	if bArr, ok := b.([]any); ok {
		if aStr, isStr := a.(string); isStr {
			return len(bArr) == 1 && awsPolicyCompare(bArr[0], aStr, unordered)
		}
	}
	switch av := a.(type) {
	case nil:
		return b == nil
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	case float64:
		bv, ok := b.(float64)
		return ok && extCompareDouble(av, bv)
	case string:
		bv, ok := b.(string)
		if !ok {
			return false
		}
		if unordered && strings.HasPrefix(av, "arn:") {
			return extFold(av, bv)
		}
		return av == bv
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		if unordered {
			for _, x := range av {
				found := false
				for _, y := range bv {
					if awsPolicyCompare(y, x, true) {
						found = true
						break
					}
				}
				if !found {
					return false
				}
			}
			return true
		}
		for i := range av {
			if !awsPolicyCompare(av[i], bv[i], unordered) {
				return false
			}
		}
		return true
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			return false
		}
		for k, v := range av {
			w, present := awsObjectGet(bv, k)
			if !present || !awsPolicyCompare(v, w, awsUnorderedFields[k]) {
				return false
			}
		}
		for k := range bv {
			if _, present := awsObjectGet(av, k); !present {
				return false
			}
		}
		return true
	}
	return false
}

// awsObjectGet looks a key up case-insensitively: an exact match first, then the smallest folding key.
func awsObjectGet(obj map[string]any, key string) (any, bool) {
	if v, ok := obj[key]; ok {
		return v, true
	}
	var candidates []string
	for k := range obj {
		if extFold(k, key) {
			candidates = append(candidates, k)
		}
	}
	if len(candidates) == 0 {
		return nil, false
	}
	sort.Strings(candidates)
	return obj[candidates[0]], true
}
