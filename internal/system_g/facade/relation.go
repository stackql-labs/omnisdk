package facade

// RelationSource supplies a read exchange whose storage and query language
// have already been resolved by the caller. Make must not perform IO.
type RelationSource interface {
	Make(inputs map[string]any) Operator
}
