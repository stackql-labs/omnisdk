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

// Provider is a provider document: the catalogue of services it offers and how calls to them
// authenticate. It is the level ABOVE a service document — the signing algorithm is declared once for
// the whole provider, so deriving it per service document would re-derive a decision already made.
type Provider interface {
	ID() string
	Name() string
	Version() string
	// Services are the services the provider offers, sorted by name.
	Services() []Service
	// Service looks one up by name.
	Service(name string) (Service, bool)
	// Security is the provider-wide scheme every call inherits.
	Security() Security
	// Credentials names the environment the document expects credentials in. Names only: a document
	// says where to look, never what the value is.
	Credentials() CredentialSource
}

// Service is one service in a provider's catalogue, and where its document lives.
type Service interface {
	Name() string
	Title() string
	Version() string
	// Ref is the service document's location as the provider states it, provider-relative.
	Ref() string
}

// CredentialSource names the environment variables a document expects credentials in.
type CredentialSource interface {
	KeyIDEnvVar() string
	SecretEnvVar() string
}

// Catalog is a whole provider bundle resolved to ADDRESSABLE exchanges: a provider document plus the
// service documents present alongside it. An address is "<provider>.<service>.<resource>", the same
// dot-path the documents give their resources, so what a caller names is what the document called it.
type Catalog interface {
	Provider() Provider
	// Services are the services whose documents are actually present, sorted. The provider lists many
	// more; a listed service with no document is not addressable, and saying so beats pretending.
	Services() []string
	// Resources are a service's resources, sorted — every one it declares, not only those with a
	// runnable SELECT.
	Resources(service string) ([]string, error)
	// Methods are a resource's methods, sorted.
	Methods(service, resource string) ([]Method, error)
	// Paths are every addressable exchange, sorted.
	Paths() []string
	// Exchange resolves one address.
	Exchange(path string) (AOTExchange, error)
}

// Method is one operation a resource declares, and the SQL verb (if any) the document maps it to.
// A method with no verb is reachable by name but not by a SQL statement.
type Method interface {
	Name() string
	// SQLVerb is select/insert/update/delete/exec, or empty when the document maps it to none.
	SQLVerb() string
	OperationID() string
}
