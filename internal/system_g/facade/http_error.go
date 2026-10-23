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

// ResponseSelectionError identifies an absent declared row key in a decoded response.
type ResponseSelectionError interface {
	error
	MissingKey() string
}

// ResponseSelectionErrorPolicy optionally treats an absent row key as an empty read.
// Returning true may also emit a host diagnostic. Other decoding errors remain fatal.
type ResponseSelectionErrorPolicy interface{ IgnoreMissingKey(key string) bool }

// HTTPObserver receives explicit document-request diagnostics. Implementations
// must be concurrency safe; callbacks must not mutate row documents.
// Authentication exchanges are outside this observer.
type HTTPObserver interface {
	HTTPRequest(method, url string, body []byte)
	HTTPResponse(status int, body []byte)
	HTTPRows(rows any)
}

// AuthenticationError marks a failed credential exchange, distinct from a
// provider resource response governed by a read-error policy.
type AuthenticationError interface {
	error
	AuthenticationFailure() bool
}
