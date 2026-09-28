// Package fn implements facade.Fn and the registry functions are looked up in. A row-producing
// function's column names are constructor input: nothing above this module can alias them.
package fn

import (
	"encoding/json"
	"fmt"
	"github.com/stackql-labs/omnisdk/pkg/sqlfn"
	"sort"
	"strconv"
	"strings"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

// registry is an immutable name → Fn lookup.
type registry struct {
	byName map[string]facade.Fn
	sorted []facade.Fn
}

// NewRegistry builds a registry over fns. A duplicate name is an error, not last-wins.
func NewRegistry(fns ...facade.Fn) (facade.FnRegistry, error) {
	byName := make(map[string]facade.Fn, len(fns))
	for _, f := range fns {
		if _, dup := byName[f.Name()]; dup {
			return nil, fmt.Errorf("fn: duplicate function %q", f.Name())
		}
		byName[f.Name()] = f
	}
	sorted := make([]facade.Fn, 0, len(byName))
	for _, f := range byName {
		sorted = append(sorted, f)
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name() < sorted[j].Name() })
	return registry{byName: byName, sorted: sorted}, nil
}

func (r registry) Fns() []facade.Fn { return append([]facade.Fn(nil), r.sorted...) }

func (r registry) Fn(name string) (facade.Fn, bool) {
	f, ok := r.byName[name]
	return f, ok
}

// Invoke resolves name to a function, picks the first signature whose arity fits and whose
// arguments all parse, and calls it. Resolution lives here so every function reports the same
// errors and Args is load-bearing rather than advisory.
func Invoke(r facade.FnRegistry, name string, args []any) (any, error) {
	f, ok := r.Fn(name)
	if !ok {
		return nil, fmt.Errorf("fn: no function %q", name)
	}
	if rc, ok := f.(rawCaller); ok {
		return rc.callRaw(args)
	}
	sigs := f.Signatures()
	if len(sigs) == 0 {
		return nil, fmt.Errorf("fn: %s declares no signatures", name)
	}
	var last error
	for i, sig := range sigs {
		if !fits(sig, len(args)) {
			continue
		}
		typed, err := coerce(sig, name, args)
		if err != nil {
			last = err
			continue
		}
		return f.Call(i, typed)
	}
	if last != nil {
		return nil, last
	}
	return nil, fmt.Errorf("fn: %s takes %s arguments, got %d", name, arities(sigs), len(args))
}

// fits reports whether a call of n arguments can use sig.
func fits(sig facade.Signature, n int) bool {
	if sig.Variadic() {
		return n >= len(sig.Args())
	}
	return n == len(sig.Args())
}

// arities renders the accepted argument counts for an error message.
func arities(sigs []facade.Signature) string {
	seen := map[string]bool{}
	var parts []string
	for _, sig := range sigs {
		s := strconv.Itoa(len(sig.Args()))
		if sig.Variadic() {
			s += " or more"
		}
		if !seen[s] {
			seen[s] = true
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, " or ")
}

// coerce parses each argument through its declared kind; a variadic signature's last kind repeats.
// NULL is passed through unparsed — it is not a value of any kind.
func coerce(sig facade.Signature, name string, args []any) ([]facade.Typed, error) {
	decl := sig.Args()
	if len(decl) == 0 {
		return nil, fmt.Errorf("fn: %s declares no argument kinds", name)
	}
	out := make([]facade.Typed, len(args))
	for i, a := range args {
		if a == nil {
			continue
		}
		k := decl[min(i, len(decl)-1)]
		lex, err := lexeme(a)
		if err != nil {
			return nil, fmt.Errorf("fn: %s argument %d: %w", name, i+1, err)
		}
		t, err := k.Parse(lex)
		if err != nil {
			return nil, fmt.Errorf("fn: %s argument %d is not %s: %w", name, i+1, k.Name(), err)
		}
		out[i] = t
	}
	return out, nil
}

// lexeme renders a Go value into the written form a Kind parses. strconv, not fmt, so an integral
// float64 renders "2" and the int kind can read it back. Structural values render as JSON.
func lexeme(v any) ([]byte, error) {
	switch t := v.(type) {
	case []byte:
		return t, nil
	case string:
		return []byte(t), nil
	case bool:
		return strconv.AppendBool(nil, t), nil
	case int:
		return strconv.AppendInt(nil, int64(t), 10), nil
	case int32:
		return strconv.AppendInt(nil, int64(t), 10), nil
	case int64:
		return strconv.AppendInt(nil, t, 10), nil
	case float32:
		return strconv.AppendFloat(nil, float64(t), 'f', -1, 32), nil
	case float64:
		return strconv.AppendFloat(nil, t, 'f', -1, 64), nil
	case json.Number:
		return []byte(t), nil
	case facade.Typed:
		return t.Lexeme(), nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("cannot render %T as a value", v)
	}
	return b, nil
}

// Builtins is the registry this module ships. out names the column a single-column table function
// emits: required input ("value" is SQLite's convention, but the caller states it).
func Builtins(out string) (facade.FnRegistry, error) {
	return BuiltinsWith(out, nil)
}

// BuiltinsWith is Builtins plus extra, a caller's own catalogue. A name extra shares with a built-in
// is an error: which one a query meant would depend on order.
func BuiltinsWith(out string, extra sqlfn.Catalog) (facade.FnRegistry, error) {
	fns := []facade.Fn{NewSplitPart(), NewStringToTable(out)}
	for _, f := range sqlfn.Builtins().Funcs() {
		fns = append(fns, FromSQL(f))
	}
	if extra != nil {
		for _, f := range extra.Funcs() {
			fns = append(fns, FromSQL(f))
		}
	}
	return NewRegistry(fns...)
}
