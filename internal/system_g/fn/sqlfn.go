package fn

import (
	"fmt"

	"github.com/stackql-labs/omnisdk/internal/kind"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/pkg/sqlfn"
)

// FromSQL adapts a catalogue function to the engine. Its signatures are declared over the unknown
// kind, one per accepted arity, so listing and row-shape checks work as for any function; calls go
// straight to it with the row values as they are, since it takes row values rather than parsed ones.
func FromSQL(f sqlfn.Func) facade.Fn { return sqlFn{f: f} }

type sqlFn struct{ f sqlfn.Func }

func (s sqlFn) Name() string { return s.f.Name() }

func (s sqlFn) Signatures() []facade.Signature {
	lo, hi := s.f.Arity()
	sig := func(n int) facade.Signature {
		args := make([]facade.Kind, n)
		for i := range args {
			args[i] = kind.Unknown()
		}
		if s.f.Shape() == sqlfn.Table {
			cols := make([]facade.FnColumn, 0, len(s.f.Columns()))
			for _, c := range s.f.Columns() {
				cols = append(cols, NewColumn(c, kind.Unknown()))
			}
			return NewRowSignature(cols, args...)
		}
		return NewSignature(kind.Unknown(), args...)
	}
	if hi < 0 {
		return []facade.Signature{Variadic(sig(max(lo, 1)))}
	}
	out := make([]facade.Signature, 0, hi-lo+1)
	for n := lo; n <= hi; n++ {
		out = append(out, sig(n))
	}
	return out
}

// Call serves a caller holding parsed values: each is turned back into its written form.
func (s sqlFn) Call(_ int, args []facade.Typed) (any, error) {
	raw := make([]any, len(args))
	for i, a := range args {
		if a != nil {
			raw[i] = string(a.Lexeme())
		}
	}
	return s.callRaw(raw)
}

func (s sqlFn) callRaw(args []any) (any, error) {
	lo, hi := s.f.Arity()
	if len(args) < lo || (hi >= 0 && len(args) > hi) {
		return nil, fmt.Errorf("fn: %s takes %s arguments, got %d", s.f.Name(), arity(lo, hi), len(args))
	}
	v, err := s.f.Call(args)
	if err != nil {
		return nil, fmt.Errorf("fn: %s: %w", s.f.Name(), err)
	}
	// The engine's row-producing contract is []any of maps.
	if rows, ok := v.([]map[string]any); ok {
		out := make([]any, len(rows))
		for i, r := range rows {
			out[i] = r
		}
		return out, nil
	}
	return v, nil
}

func arity(lo, hi int) string {
	switch {
	case hi < 0:
		return fmt.Sprintf("%d or more", lo)
	case lo == hi:
		return fmt.Sprint(lo)
	}
	return fmt.Sprintf("%d to %d", lo, hi)
}

// rawCaller is a function taking row values unparsed.
type rawCaller interface{ callRaw([]any) (any, error) }
