package facade

type ResourceBindingResolver interface {
	Resolve(value any, sources map[string]map[string]any) (any, error)
	References(value any, source string) ([]string, error)
}
