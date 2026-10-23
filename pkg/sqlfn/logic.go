package sqlfn

import "fmt"

type deferredScalar struct {
	Func
	evaluate func([]func() (any, error)) (any, error)
}

func newDeferredScalar(name string, minimum, maximum int, evaluate func([]func() (any, error)) (any, error)) Func {
	base := NewScalar(name, minimum, maximum, func(args []any) (any, error) {
		thunks := make([]func() (any, error), len(args))
		for i, value := range args {
			thunks[i] = func() (any, error) { return value, nil }
		}
		return evaluate(thunks)
	})
	return deferredScalar{Func: base, evaluate: evaluate}
}

func (f deferredScalar) CallLazy(args []func() (any, error)) (any, error) {
	minimum, maximum := f.Arity()
	if len(args) < minimum || maximum >= 0 && len(args) > maximum {
		return nil, fmt.Errorf("%s: invalid argument count %d", f.Name(), len(args))
	}
	return f.evaluate(args)
}

func booleanFunctions(truth func(any) (bool, error), boolean func(bool) any) []Func {
	functions := []Func{newDeferredScalar("not", 1, 1, func(args []func() (any, error)) (any, error) {
		value, err := args[0]()
		if err != nil || value == nil {
			return nil, err
		}
		test, err := truth(value)
		return boolean(!test), err
	})}
	for _, name := range []string{"and", "or"} {
		functions = append(functions, newDeferredScalar(name, 2, 2, func(args []func() (any, error)) (any, error) {
			unknown := false
			decisive := name == "or"
			for _, arg := range args {
				value, err := arg()
				if err != nil {
					return nil, err
				}
				if value == nil {
					unknown = true
					continue
				}
				test, err := truth(value)
				if err != nil {
					return nil, err
				}
				if test == decisive {
					return boolean(test), nil
				}
			}
			if unknown {
				return nil, nil
			}
			return boolean(!decisive), nil
		}))
	}
	return functions
}
