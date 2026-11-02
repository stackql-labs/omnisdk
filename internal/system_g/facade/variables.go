package facade

type VariableResolver interface {
	Declared() bool
	Resolve(name string, lookup func(string) (string, bool)) (string, bool, error)
}
