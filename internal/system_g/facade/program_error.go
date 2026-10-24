package facade

// UnsupportedProgramError names a declared program language without an evaluator.
type UnsupportedProgramError interface {
	error
	ProgramType() string
}
