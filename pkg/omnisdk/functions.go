package omnisdk

import (
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/fn"
)

// Function is one SQL-visible function: a consumer such as stackql lists these once and registers
// each into its own evaluator, so a function added here ships without a change on the consumer's
// side. Scalar and table functions are one interface — multiplicity is a property of the return.
type Function interface {
	Name() string
	// Signatures are the argument lists this name accepts, in resolution order. One name may
	// accept several; the first whose arity fits and whose arguments parse is the one that runs.
	Signatures() []Signature
}

// Signature is one accepted argument list and what it yields. Kinds are published by NAME from the
// SDK's type vocabulary ("string", "int", "decimal", "bool", "object", "unknown") so the
// declaration survives being marshalled to a consumer in another process.
type Signature struct {
	// Args are the argument kinds in order; with Variadic, the last repeats.
	Args     []string `json:"args"`
	Variadic bool     `json:"variadic,omitempty"`
	// Returns is the result kind: the value's kind for a scalar, "object" for row-producing.
	Returns string `json:"returns"`
	// Columns are the output columns of a row-producing signature; nil for a scalar.
	Columns []Column `json:"columns,omitempty"`
}

// Column is one output column of a row-producing signature.
type Column struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// FunctionCatalog is the discovery + invocation seam for functions, the sibling of Catalog: a
// function takes evaluated arguments rather than scope, and returns a value rather than a Plan.
type FunctionCatalog interface {
	// Functions lists every function, ordered by name.
	Functions() []Function
	// GetFunction returns one by name.
	GetFunction(name string) (Function, bool)
	// Call resolves the signature, parses each argument through its declared kind, and evaluates.
	// It is the only way in, so a consumer cannot reach an unchecked call.
	Call(name string, args []any) (any, error)
}

// Functions returns the built-in function catalog. rowColumn is the column name a single-column
// table function emits (SQLite's json_each calls it "value"): required explicit input, because
// nothing above this module can rename it afterwards.
func Functions(rowColumn string) (FunctionCatalog, error) {
	r, err := fn.Builtins(rowColumn)
	if err != nil {
		return nil, err
	}
	return functions{reg: r}, nil
}

type functions struct{ reg facade.FnRegistry }

func (f functions) Functions() []Function {
	in := f.reg.Fns()
	out := make([]Function, 0, len(in))
	for _, x := range in {
		out = append(out, published{fn: x})
	}
	return out
}

func (f functions) GetFunction(name string) (Function, bool) {
	x, ok := f.reg.Fn(name)
	if !ok {
		return nil, false
	}
	return published{fn: x}, true
}

func (f functions) Call(name string, args []any) (any, error) { return fn.Invoke(f.reg, name, args) }

// published is one internal function as a consumer sees it.
type published struct{ fn facade.Fn }

func (p published) Name() string { return p.fn.Name() }

func (p published) Signatures() []Signature {
	in := p.fn.Signatures()
	out := make([]Signature, 0, len(in))
	for _, s := range in {
		sig := Signature{Variadic: s.Variadic(), Returns: s.Returns().Name()}
		for _, k := range s.Args() {
			sig.Args = append(sig.Args, k.Name())
		}
		for _, c := range s.Columns() {
			sig.Columns = append(sig.Columns, Column{Name: c.Name(), Kind: c.Kind().Name()})
		}
		out = append(out, sig)
	}
	return out
}
