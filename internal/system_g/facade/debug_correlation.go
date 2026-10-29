package facade

// DebugCorrelationBinding maps an opt-in label to provider-declared request inputs and live state.
type DebugCorrelationBinding interface {
	Target() string
	Attribute() string
	CreateParams() map[string]string
}
