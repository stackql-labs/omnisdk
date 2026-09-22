package facade

// Fn is a named function over already-evaluated arguments. Scalar and table functions are one
// interface: multiplicity is a property of the return, not the type.
//
//   - scalar → a Go scalar
//   - record → map[string]any
//   - table  → []any of map[string]any
//
// Table rows are always objects; a bare scalar row has no addressable field. Column names are fixed
// at construction — there is no SQL engine here to alias them.
type Fn interface {
	Name() string
	// Signatures are the argument lists this name accepts, in RESOLUTION order: the invoker takes
	// the first whose arity fits and whose arguments all parse. Order is the tie-break, not a
	// specificity rule, so a permissive signature (Unknown, String) must be declared last.
	Signatures() []Signature
	// Call evaluates under Signatures()[sig], over arguments already parsed to that signature's
	// kinds. A nil element is NULL.
	Call(sig int, args []Typed) (any, error)
}

// Signature is one accepted argument list and what it yields. An optional trailing argument is a
// second signature, not a flag.
type Signature interface {
	// Args are the argument kinds, in order. Enforced: the invoker parses each argument through its
	// kind before Call. When Variadic, the last kind repeats for every further argument.
	Args() []Kind
	Variadic() bool
	// Returns is the result kind: the value's kind for a scalar, Object for anything row-producing.
	Returns() Kind
	// Columns are the output columns of a row-producing signature; nil for a scalar.
	Columns() []FnColumn
}

// FnColumn is one output column: its published name and the kind of its values.
type FnColumn interface {
	Name() string
	Kind() Kind
}

// FnRegistry is an immutable name → Fn lookup, shareable across runs.
type FnRegistry interface {
	// Fns lists every function, ordered by name.
	Fns() []Fn
	Fn(name string) (Fn, bool)
}
