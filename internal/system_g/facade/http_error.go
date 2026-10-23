package facade

// HTTPStatusError is a provider response with a non-success status, distinct
// from a transport failure that produced no response.
type HTTPStatusError interface {
	error
	StatusCode() int
	ResponseBody() string
	RequestURL() string
}

// HTTPReadErrorPolicy explicitly tolerates selected upstream failures for reads.
// It never applies to mutations or authentication exchanges.
type HTTPReadErrorPolicy interface{ IsIgnored(statusCode int) bool }

// HTTPReadErrorObserver receives ignored responses for host-owned diagnostics.
type HTTPReadErrorObserver interface {
	IgnoredStatus(statusCode int, body string)
}
