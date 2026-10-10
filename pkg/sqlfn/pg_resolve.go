package sqlfn

import (
	"strings"
)

// Postgres's overload resolution, ported from parse_func.c (func_get_detail, func_match_argtypes,
// func_select_candidate) and parse_coerce.c (can_coerce_type), over the catalog data generated into
// pg_catalog_gen.go. Given a call's argument types it picks the overload Postgres would pick, or
// reports what Postgres would report.

// pgTypeCategory is a type's pg_type category and whether it is that category's preferred type.
type pgTypeCategory struct {
	cat       string
	preferred bool
}

// pgProc is one pg_proc overload: its implementation's symbol, argument types, result type, whether
// it returns a set, and the element type of a variadic last argument.
type pgProc struct {
	src      string
	args     []string
	ret      string
	retset   bool
	variadic string
	strict   bool     // NULL in any argument is a NULL result, without the call
	outs     []string // the output columns of a set-returning function with OUT parameters
	defaults []string // the input literals of the trailing arguments' defaults
}

// signature is the overload's identity, as name(argtypes), for dispatch.
func (p pgProc) signature(name string) string { return name + "(" + strings.Join(p.args, ",") + ")" }

// pgCandidate is an overload expanded for one call: the type each argument is taken as.
type pgCandidate struct {
	proc pgProc
	args []string
	// fill are the trailing arguments the call leaves to their defaults, as input literals.
	fill []string
}

// pgTypeNameOf is the catalog name of a value's type.
func pgTypeNameOf(v any) string {
	switch pgTypeOf(v) {
	case pgTypeUnknown:
		return "unknown"
	case pgTypeText:
		return "text"
	case pgTypeInt4:
		return "int4"
	case pgTypeInt8:
		return "int8"
	case pgTypeNumeric:
		return "numeric"
	case pgTypeFloat8:
		return "float8"
	case pgTypeBool:
		return "bool"
	case pgTypeJSON:
		return "json"
	case pgTypeJSONB:
		return "jsonb"
	case pgTypeTextArray:
		return "_text"
	case pgTypeInt4Array:
		return "_int4"
	case pgTypeTimestamptz:
		return "timestamptz"
	case pgTypeTimestamp:
		return "timestamp"
	case pgTypeDate:
		return "date"
	case pgTypeInterval:
		return "interval"
	}
	return "unknown"
}

func pgCategory(t string) pgTypeCategory {
	if c, ok := pgTypeInfo[t]; ok {
		return c
	}
	return pgTypeCategory{cat: "U"}
}

func isPolymorphic(t string) bool {
	switch t {
	case "anyelement", "anyarray", "anynonarray", "anyenum", "anyrange", "anymultirange",
		"anycompatible", "anycompatiblearray", "anycompatiblenonarray", "anycompatiblerange",
		"anycompatiblemultirange":
		return true
	}
	return false
}

func isArrayType(t string) bool { return strings.HasPrefix(t, "_") }

// canCoerce is can_coerce_type in implicit context.
func canCoerce(in, target []string) bool {
	generics := false
	for i := range in {
		switch {
		case in[i] == target[i], target[i] == "any":
		case isPolymorphic(target[i]):
			generics = true
		case in[i] == "unknown":
		case pgImplicitCasts[[2]string{in[i], target[i]}]:
		default:
			return false
		}
	}
	return !generics || genericConsistent(in, target)
}

// genericConsistent is check_generic_type_consistency for the polymorphic types in use: every
// anyelement argument the same type, anyarray an array of it, anynonarray not an array, and the
// anycompatible family having a common type.
func genericConsistent(in, target []string) bool {
	elem, arr := "", ""
	var compat []string
	for i, t := range target {
		a := in[i]
		switch t {
		case "anyelement", "anynonarray":
			if a == "unknown" {
				continue
			}
			if t == "anynonarray" && isArrayType(a) {
				return false
			}
			if elem != "" && elem != a {
				return false
			}
			elem = a
		case "anyarray":
			if a == "unknown" {
				continue
			}
			if !isArrayType(a) {
				return false
			}
			if arr != "" && arr != a {
				return false
			}
			arr = a
		case "anycompatible", "anycompatiblenonarray":
			if a == "unknown" {
				continue
			}
			if t == "anycompatiblenonarray" && isArrayType(a) {
				return false
			}
			compat = append(compat, a)
		case "anycompatiblearray":
			if a == "unknown" {
				continue
			}
			if !isArrayType(a) {
				return false
			}
			compat = append(compat, a[1:])
		case "anyenum", "anyrange", "anymultirange", "anycompatiblerange", "anycompatiblemultirange":
			return false
		}
	}
	if elem != "" && arr != "" && arr != "_"+elem {
		return false
	}
	if len(compat) > 0 {
		if _, ok := selectCommonType(compat); !ok {
			return false
		}
	}
	return true
}

// selectCommonType is select_common_type over known types: the first type, replaced by each later
// one of the same category that is preferred or to which it casts implicitly and not back.
func selectCommonType(types []string) (string, bool) {
	ptype := ""
	for _, t := range types {
		if t == "unknown" {
			continue
		}
		if ptype == "" {
			ptype = t
			continue
		}
		if t == ptype {
			continue
		}
		pc, nc := pgCategory(ptype), pgCategory(t)
		if pc.cat != nc.cat {
			return "", false
		}
		if !pc.preferred && pgImplicitCasts[[2]string{ptype, t}] && !pgImplicitCasts[[2]string{t, ptype}] {
			ptype = t
		}
	}
	if ptype == "" {
		return "text", true
	}
	return ptype, true
}

// expand is FuncnameGetCandidates for one call: each overload whose arity fits, a variadic one with
// its variadic element repeated to fill the call.
func expand(procs []pgProc, nargs int) []pgCandidate {
	var out []pgCandidate
	for _, p := range procs {
		switch {
		case p.variadic != "" && p.variadic != "0":
			// A variadic parameter takes at least one argument (a zero-argument form is its own
			// overload); its element type fills the call.
			if nargs < len(p.args) {
				continue
			}
			args := append([]string(nil), p.args[:len(p.args)-1]...)
			for len(args) < nargs {
				args = append(args, p.variadic)
			}
			out = append(out, pgCandidate{proc: p, args: args})
		case len(p.args) == nargs:
			out = append(out, pgCandidate{proc: p, args: p.args})
		case nargs < len(p.args) && len(p.args)-nargs <= len(p.defaults):
			out = append(out, pgCandidate{proc: p, args: p.args[:nargs], fill: p.defaults[len(p.defaults)-(len(p.args)-nargs):]})
		}
	}
	return out
}

// resolvePg is func_get_detail without the type-coercion interpretation: the overload of name that
// Postgres chooses for arguments of types in.
func resolvePg(name string, in []string) (pgCandidate, error) {
	c, err := selectPg(name, in)
	if err != nil {
		return c, err
	}
	return c, checkPolymorphic(c, in)
}

// checkPolymorphic is enforce_generic_type_consistency's refusal: an anyelement-family parameter
// whose every argument is an untyped literal leaves its type undetermined. (An anycompatible one
// resolves to text.)
func checkPolymorphic(c pgCandidate, in []string) error {
	family, known := false, false
	for i, t := range c.args {
		switch t {
		case "anyelement", "anyarray", "anynonarray", "anyenum", "anyrange", "anymultirange":
			family = true
			known = known || in[i] != "unknown"
		}
	}
	if family && !known {
		return pgErrorf("could not determine polymorphic type because input has type unknown")
	}
	return nil
}

// selectPg is func_get_detail's choice of overload.
func selectPg(name string, in []string) (pgCandidate, error) {
	raw := expand(pgProcs[name], len(in))
	for _, c := range raw {
		if equalTypes(c.args, in) {
			return c, nil
		}
	}
	var matched []pgCandidate
	for i := len(raw) - 1; i >= 0; i-- { // func_match_argtypes builds its list in reverse
		if canCoerce(in, raw[i].args) {
			matched = append(matched, raw[i])
		}
	}
	switch len(matched) {
	case 0:
		return pgCandidate{}, pgErrorf("function %s(%s) does not exist", name, displayTypes(in))
	case 1:
		return matched[0], nil
	}
	if c, ok := selectCandidate(in, matched); ok {
		return c, nil
	}
	return pgCandidate{}, pgErrorf("function %s(%s) is not unique", name, displayTypes(in))
}

func equalTypes(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func displayTypes(in []string) string {
	names := make([]string, len(in))
	for i, t := range in {
		names[i] = pgDisplayName(t)
	}
	return strings.Join(names, ", ")
}

// pgDisplayName is format_type's name for a catalog type.
func pgDisplayName(t string) string {
	switch t {
	case "int4":
		return "integer"
	case "int8":
		return "bigint"
	case "float8":
		return "double precision"
	case "bool":
		return "boolean"
	case "timestamptz":
		return "timestamp with time zone"
	case "timestamp":
		return "timestamp without time zone"
	}
	if isArrayType(t) {
		return pgDisplayName(t[1:]) + "[]"
	}
	return t
}

// selectCandidate is func_select_candidate.
func selectCandidate(in []string, cands []pgCandidate) (pgCandidate, bool) {
	nunknowns := 0
	for _, t := range in {
		if t == "unknown" {
			nunknowns++
		}
	}
	keepBest := func(cands []pgCandidate, score func(pgCandidate) int) []pgCandidate {
		best := -1
		var out []pgCandidate
		for _, c := range cands {
			n := score(c)
			if n > best || out == nil {
				best = n
				out = []pgCandidate{c}
			} else if n == best {
				out = append(out, c)
			}
		}
		return out
	}
	cands = keepBest(cands, func(c pgCandidate) int {
		n := 0
		for i, t := range in {
			if t != "unknown" && c.args[i] == t {
				n++
			}
		}
		return n
	})
	if len(cands) == 1 {
		return cands[0], true
	}
	cats := make([]string, len(in))
	for i, t := range in {
		cats[i] = pgCategory(t).cat
	}
	cands = keepBest(cands, func(c pgCandidate) int {
		n := 0
		for i, t := range in {
			if t == "unknown" {
				continue
			}
			ct := pgCategory(c.args[i])
			if c.args[i] == t || (ct.preferred && ct.cat == cats[i]) {
				n++
			}
		}
		return n
	})
	if len(cands) == 1 {
		return cands[0], true
	}
	if nunknowns == 0 {
		return pgCandidate{}, false
	}
	// Resolve each unknown position's category: STRING if any candidate takes a string there,
	// else the one category all candidates agree on.
	resolved := true
	slotCat := make([]string, len(in))
	slotPref := make([]bool, len(in))
	for i, t := range in {
		if t != "unknown" {
			continue
		}
		conflict := false
		slotCat[i] = ""
		for _, c := range cands {
			ct := pgCategory(c.args[i])
			switch {
			case slotCat[i] == "":
				slotCat[i] = ct.cat
				slotPref[i] = ct.preferred
			case ct.cat == slotCat[i]:
				slotPref[i] = slotPref[i] || ct.preferred
			case ct.cat == "S":
				slotCat[i] = ct.cat
				slotPref[i] = ct.preferred
			default:
				conflict = true
			}
		}
		if conflict && slotCat[i] != "S" {
			resolved = false
			break
		}
	}
	if resolved {
		var kept []pgCandidate
		for _, c := range cands {
			keep := true
			for i, t := range in {
				if t != "unknown" {
					continue
				}
				ct := pgCategory(c.args[i])
				if ct.cat != slotCat[i] || (slotPref[i] && !ct.preferred) {
					keep = false
					break
				}
			}
			if keep {
				kept = append(kept, c)
			}
		}
		if len(kept) > 0 {
			cands = kept
		}
		if len(cands) == 1 {
			return cands[0], true
		}
	}
	if nunknowns < len(in) {
		known := ""
		for _, t := range in {
			if t == "unknown" {
				continue
			}
			if known == "" {
				known = t
			} else if known != t {
				known = ""
				break
			}
		}
		if known != "" {
			same := make([]string, len(in))
			for i := range same {
				same[i] = known
			}
			var unique []pgCandidate
			for _, c := range cands {
				if canCoerce(same, c.args) {
					unique = append(unique, c)
					if len(unique) > 1 {
						break
					}
				}
			}
			if len(unique) == 1 {
				return unique[0], true
			}
		}
	}
	return pgCandidate{}, false
}
