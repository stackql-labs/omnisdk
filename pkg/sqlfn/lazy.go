package sqlfn

import "fmt"

type lazyScalar struct {
	name  string
	min   int
	odd   bool
	truth func(any) (bool, error)
}

func newChoice(name string, minimum int, odd bool, truth func(any) (bool, error)) Func {
	return lazyScalar{name: name, min: minimum, odd: odd, truth: truth}
}

func (f lazyScalar) Name() string      { return f.name }
func (f lazyScalar) Arity() (int, int) { return f.min, -1 }
func (f lazyScalar) Shape() Shape      { return Scalar }
func (f lazyScalar) Columns() []string { return nil }
func (f lazyScalar) Call(args []any) (any, error) {
	thunks := make([]func() (any, error), len(args))
	for i, value := range args {
		thunks[i] = func() (any, error) { return value, nil }
	}
	return f.CallLazy(thunks)
}
func (f lazyScalar) CallLazy(args []func() (any, error)) (any, error) {
	if len(args) < f.min || f.odd && len(args)%2 == 0 {
		return nil, fmt.Errorf("%s requires condition/value pairs and an optional fallback", f.name)
	}
	i := 0
	for ; i+1 < len(args); i += 2 {
		condition, err := args[i]()
		if err != nil {
			return nil, err
		}
		matched, err := f.truth(condition)
		if err != nil {
			return nil, err
		}
		if matched {
			return args[i+1]()
		}
	}
	if i < len(args) {
		return args[i]()
	}
	return nil, nil
}

func sqliteTruth(value any) (bool, error) {
	truth, _ := truthy(value)
	return truth, nil
}

// Simple CASE evaluates its base once, even when it has several WHEN clauses.
func newSimpleCase(equal func(any, any) (bool, error)) Func {
	return newDeferredScalar("case_value", 4, -1, func(args []func() (any, error)) (any, error) {
		if len(args)%2 != 0 {
			return nil, fmt.Errorf("case_value requires a base, WHEN/value pairs and a fallback")
		}
		base, err := args[0]()
		if err != nil {
			return nil, err
		}
		if base != nil {
			for i := 1; i+1 < len(args); i += 2 {
				when, err := args[i]()
				if err != nil {
					return nil, err
				}
				matched, err := equal(base, when)
				if err != nil {
					return nil, err
				}
				if matched {
					return args[i+1]()
				}
			}
		}
		return args[len(args)-1]()
	})
}

func pgTruth(value any) (bool, error) {
	if value == nil {
		return false, nil
	}
	// SQL conditions accept booleans and untyped boolean literals, not an
	// implicit cast of a numeric or text column to boolean.
	if pgOperandType(value) != "bool" && pgOperandType(value) != "unknown" {
		return false, fmt.Errorf("CASE condition must be boolean")
	}
	coerced, err := pgCoerce(pgEnv{}, value, "bool", nil, nil)
	if err != nil {
		return false, err
	}
	boolean, ok := coerced.(bool)
	if !ok {
		return false, fmt.Errorf("CASE condition must be boolean")
	}
	return boolean, nil
}
