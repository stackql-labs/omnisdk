package facade

// PartialUpdateMethod identifies an operation that changes supplied fields
// without replacing the whole resource.
type PartialUpdateMethod interface{ PartialUpdate() bool }
