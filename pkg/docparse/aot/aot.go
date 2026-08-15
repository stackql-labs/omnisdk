// Package aot is the contract between a provider-document parser and whatever compiles or runs what
// it found. An AOT exchange is a DESCRIPTION — what the document says a call is, ahead of any plan.
//
// It lives in its own package for one reason: a compiler must depend on the CONTRACT, never on a
// dialect. stackql documents are one dialect; plain OpenAPI, a discovery document or a provider's own
// format are others, and each can implement this without the compiler learning they exist.
//
// Deliberately absent: anything executable. Binding a call to an operator, and deciding where a
// declared transform runs, are plan-time concerns — a document attaches its response program to the
// SOURCE while a plan applies transforms toward the SINK, and that relocation is a decision a
// compiler makes against these interfaces.
package aot

// AOTExchange is one operation as the document declares it, ahead of any plan.
type AOTExchange interface {
	// Name identifies the exchange (the resource it serves, e.g. "instances").
	Name() string
	// Inputs are the values the call needs bound — server variables and required parameters.
	Inputs() []string
	// OperationID is the document's own name for the backing operation.
	OperationID() string
	// Request is the call to make.
	Request() Request
	// Response is how the document says to read what comes back.
	Response() Response
	// Security is how the document says the call is authenticated. A compiler applies it implicitly:
	// a document that says its calls are SigV4-signed means every one of them, and requiring a caller
	// to restate that per exchange is how signing gets forgotten.
	Security() Security
}

// Scheme is a normalized authentication scheme — normalized because a document expresses it in its
// own dialect's terms, and a compiler must not learn those.
type Scheme string

const (
	// SchemeNone is an unauthenticated call, or one the document does not describe.
	SchemeNone Scheme = ""
	// SchemeAWSSigV4 is AWS Signature V4 request signing.
	SchemeAWSSigV4 Scheme = "aws.sigv4"
)

// Security is the declared authentication for a call.
type Security interface {
	Scheme() Scheme
	// Name is the document's own name for the scheme, kept for diagnostics.
	Name() string
}

// Request is the declared call: a URL template, verb, and the body/query the document specifies.
type Request interface {
	Method() string
	// URL is a template; {name} placeholders are bound from Inputs.
	URL() string
	MediaType() string
	// Params are the body parameters the document declares, already stripped of its own markers.
	Params() map[string]string
}

// Response is how the document says to read the reply. MediaType is what the wire carries;
// OverrideMediaType is what the declared Transform turns it into.
type Response interface {
	MediaType() string
	OverrideMediaType() string
	// ObjectKey is the path to the item list WITHIN the transformed body — so it is meaningful only
	// after Transform has run.
	ObjectKey() string
	// Transform is the document's response transform, attached here at the SOURCE. A compile step
	// decides where it actually runs.
	Transform() Transform
	Pagination() Pagination
}

// Transform is a declared body transformation: a program and the language it is written in.
type Transform interface {
	// Type names the evaluator (e.g. golang_template_mxj_v0.2.0). Empty means none declared.
	Type() string
	Body() string
}

// Pagination is how the document says pages continue. Token keys are paths into the TRANSFORMED
// body, and locations say whether a token travels in the body or the query.
type Pagination interface {
	// RequestToken is the key and location the next-page token is SENT as.
	RequestToken() (key, location string)
	// ResponseToken is the key and location the next-page token is READ from.
	ResponseToken() (key, location string)
	// Declared reports whether the document specifies pagination at all.
	Declared() bool
}
