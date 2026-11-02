package facade

// ServerInputResolver applies declared endpoint policy to caller-supplied inputs.
// Resolve returns additional bindings; Required reports inputs still needed.
// Environment access is injected by the host, never read implicitly.
type ServerInputResolver interface {
	Resolve(supplied func(string) bool) (map[string]string, error)
	Required(names []string, supplied func(string) bool) ([]string, error)
}
