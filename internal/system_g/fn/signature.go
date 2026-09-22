package fn

import (
	"github.com/stackql-labs/omnisdk/internal/kind"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

// objectKind is the return of every row-producing signature.
var objectKind = kind.Object()

var (
	_ facade.Signature = signature{}
	_ facade.FnColumn  = column{}
)

type signature struct {
	args     []facade.Kind
	variadic bool
	returns  facade.Kind
	cols     []facade.FnColumn
}

// NewSignature declares a scalar signature: argument kinds in order, and the result kind.
func NewSignature(returns facade.Kind, args ...facade.Kind) facade.Signature {
	return signature{args: args, returns: returns}
}

// NewRowSignature declares a row-producing signature with its output columns.
func NewRowSignature(cols []facade.FnColumn, args ...facade.Kind) facade.Signature {
	return signature{args: args, returns: objectKind, cols: cols}
}

// Variadic returns sig with its last argument kind repeating.
func Variadic(sig facade.Signature) facade.Signature {
	return signature{args: sig.Args(), variadic: true, returns: sig.Returns(), cols: sig.Columns()}
}

func (s signature) Args() []facade.Kind        { return s.args }
func (s signature) Variadic() bool             { return s.variadic }
func (s signature) Returns() facade.Kind       { return s.returns }
func (s signature) Columns() []facade.FnColumn { return s.cols }

type column struct {
	name string
	kind facade.Kind
}

// NewColumn declares an output column.
func NewColumn(name string, k facade.Kind) facade.FnColumn { return column{name: name, kind: k} }

func (c column) Name() string      { return c.name }
func (c column) Kind() facade.Kind { return c.kind }
