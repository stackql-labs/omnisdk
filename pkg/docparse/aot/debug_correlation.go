package aot

type DebugCorrelationBinding interface {
	Target() string
	Attribute() string
	CreateParams() map[string]string
}

type DebugCorrelations interface {
	DebugCorrelationBindings(resource string) ([]DebugCorrelationBinding, error)
}
