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

import "time"

// ParameterDefault exposes a declared wire default without making request
// scope optional. Compilers may apply it to non-scope metadata such as headers.
type ParameterDefault interface{ Default() any }

// ServerBindings retains declared server variables after endpoint rewriting.
type ServerBindings interface{ ServerVariables() []string }

// ServerEnvironmentBindings exposes only explicit environment-variable annotations.
// Parsers retain metadata; only a host may resolve environment values and pass them
// as explicit query inputs. The library never reads these environment variables.
type ServerEnvironmentBindings interface{ ServerEnvironmentBindings() map[string]string }

// RequestFallback supplies a complete body only when no body fields were supplied.
type RequestFallback interface{ DefaultBody() string }

// View is a document-declared SQL projection and its dialect fallback.
type View interface {
	DDL() string
	Predicate() string
	Fallback() (View, bool)
}

// RequiredSchema lists required fields of a declared object schema.
type RequiredSchema interface{ RequiredProperties() []string }

// HTTPRetry declares a document's retry bounds and transient statuses.
type HTTPRetry interface {
	Attempts() int
	InitialDelay() time.Duration
	MaxDelay() time.Duration
	Multiplier() float64
	Algorithm() string
	Methods() []string
	Statuses() []int
}
type RetryExchange interface{ Retry() (HTTPRetry, bool) }

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
	// Beta is the call the document says must follow this one before what it made exists, false
	// where it declares none.
	Beta() (Beta, bool)
	// Pushdown is what the call can do server-side that a query would otherwise do to the rows it
	// returns, false where the document declares nothing.
	Pushdown() (Pushdown, bool)
}

// Pushdown is a call's server-side query options: projection, filtering and a row cap. Each is
// optional, and an option the call lacks is done client-side.
type Pushdown interface {
	Select() (PushdownOption, bool)
	Filter() (PushdownOption, bool)
	Top() (PushdownOption, bool)
}

// PushdownOption is one server-side query option as the document declares it.
type PushdownOption interface {
	// Dialect is the syntax family the option is written in: odata, or custom.
	Dialect() string
	// ParamName is the query parameter carrying it; empty where none is known.
	ParamName() string
	// Delimiter separates a projection's columns.
	Delimiter() string
	// MaxValue caps a row cap; zero is no cap.
	MaxValue() int
	// SupportsColumn and SupportsOperator report what the option may name; a document that lists
	// none allows any.
	SupportsColumn(name string) bool
	SupportsOperator(op string) bool
}

// Beta is a β edge a document declares from an operation to the call that completes it: a create
// answered with a long-running operation, and the get that reads that operation until it is done.
// An operation declaring one has not finished when it answers; it has finished when Await says so.
type Beta interface {
	// Method is the call that follows.
	Method() AOTExchange
	// Bind maps each of Method's inputs to the field of this operation's response that supplies it.
	// Inputs not named here are the same values this operation was called with — its scope.
	Bind() map[string]string
	Await() Await
}

// Await is when a followed call reports done. Every field is required: how long to wait is the
// document's statement about its API, never a default.
type Await interface {
	// StatusPath is the dotted path of the status in Method's response.
	StatusPath() string
	// Done is the status value that means finished.
	Done() string
	// ErrorPath is the dotted path of an error a finished operation reports; empty where the API
	// reports none. Finished with an error is failed.
	ErrorPath() string
	Interval() time.Duration
	// MaxAttempts bounds the wait. Exhausting it says nothing about whether the operation succeeded.
	MaxAttempts() int
}

// Scheme is a normalized authentication scheme — normalized because a document expresses it in its
// own dialect's terms, and a compiler must not learn those.
type Scheme string

const (
	// SchemeNone is an unauthenticated call, or one the document does not describe.
	SchemeNone Scheme = ""
	SchemeOCI  Scheme = "oci.signing_v1"
	// SchemeAWSSigV4 is AWS Signature V4 request signing.
	SchemeAWSSigV4 Scheme = "aws.sigv4"
	// SchemeServiceAccount is a Google service-account key exchanged for an OAuth access token.
	SchemeServiceAccount Scheme = "google.service_account"
	// SchemeOAuthClientCredentials is an OAuth2 client-credentials grant: an id and secret exchanged
	// for a bearer token. Azure declares it, and so does any provider whose documents say oauth2
	// without naming a dialect.
	SchemeOAuthClientCredentials Scheme = "oauth2.client_credentials"
	// SchemeBasic is HTTP basic auth: a username and password, or a pre-encoded pair.
	SchemeBasic Scheme = "http.basic"
	// SchemeBearer is a static bearer token in the Authorization header.
	SchemeBearer Scheme = "http.bearer"
	// SchemeAPIKey is a key sent in a named header or query parameter.
	SchemeAPIKey Scheme = "api_key"
)

// TypedSchema is implemented by a Schema that knows its declared type: OpenAPI's type ("string",
// "integer", "number", "boolean", "object", "array") and format ("int64", "date-time", …). Either is
// empty where the document declares none.
type TypedSchema interface {
	Type() string
	Format() string
}

// AuthDefaults is what a provider document says about authenticating beyond its scheme: where a key
// goes, what prefixes it, and which environment variables hold the credentials. A caller's own
// settings take precedence field by field; these fill what the caller left empty.
type AuthDefaults struct {
	TenancyOCIDEnvVar    string
	UserOCIDEnvVar       string
	FingerprintEnvVar    string
	PrivateKeyPathEnvVar string
	Type                 string
	Name                 string // header or query parameter carrying a key
	Location             string // "header" (default) or "query"
	ValuePrefix          string
	CredentialsEnvVar    string
	UsernameEnvVar       string
	PasswordEnvVar       string
	// OAuth2 client credentials: where the id and secret are found, the token endpoint, and the
	// scopes asked for. TokenURL may reference the environment as {{ .__env__NAME }}.
	ClientIDEnvVar     string
	ClientSecretEnvVar string
	TokenURL           string
	Scopes             []string
	GrantType          string
	// Successor is a further scheme applied after this one — a second header, for an API that
	// wants two keys.
	Successor *AuthDefaults
}

// AuthConfigured is implemented by a Provider whose document states AuthDefaults.
type AuthConfigured interface {
	AuthDefaults() AuthDefaults
}

// Security is the declared authentication for a call.
type Security interface {
	Scheme() Scheme
	// Name is the document's own name for the scheme, kept for diagnostics.
	Name() string
}

// Request is the declared call: a URL template, verb, and the parameters the document specifies.
type Request interface {
	Method() string
	// URL is a template; {name} placeholders are bound from Inputs.
	URL() string
	MediaType() string
	// Params are the fixed BODY parameters the document states outright (an action name, an API
	// version) — values, not inputs.
	Params() map[string]string
	// Parameters are the inputs the operation accepts, each with WHERE it belongs on the wire. A name
	// alone is not enough: the same parameter is a path segment for one operation and a query string
	// for another, and a caller's value cannot be placed without knowing which.
	Parameters() []Parameter
	// BodyMediaType is what the operation's request body is written as, empty where it takes none.
	//
	// The body's CONTENT is the caller's — an intent document, the object to create — so the
	// document does not need to enumerate its fields for one to be sent. Saying the operation takes
	// a body, and in what encoding, is the whole of what a compiler needs.
	BodyMediaType() string
	// Translate names how the described request is sent instead, empty where it is sent as
	// described: get_query_to_post_form_utf_8 sends a GET's query as a form POST's body.
	Translate() string
	// Transpose names how a structured input is written into the query, empty where it is sent as one
	// value: AWSCanonical writes a list as name.1, name.2 and a map as name.Key.
	Transpose() string
}

// RequestShape carries optional schema and naming declarations for an operation.
type RequestShape interface {
	NativeCasing() string
	SnakeCaseAliases() bool
	BodySchema() Schema
}

// SigningRequest retains the service identity when an endpoint is overridden.
// A private endpoint's hostname need not name the service used in SigV4 scope.
type SigningRequest interface {
	SigningService() string
}

// BodyTranslationRequest declares a mapping of flat body inputs into a nested
// request property (for example naive_DesiredState).
type BodyTranslationRequest interface {
	BodyTranslation() string
}

// XMLRequest declares the wire envelope independently of the body properties.
type XMLRequest interface {
	XMLRootName() string
	XMLDeclaration() string
	XMLRootAnnotation() string
}

// StringOnlySchema marks a structured value serialized as JSON text on wire.
type StringOnlySchema interface {
	StringOnly() bool
}

// Parameter is one declared input and its location on the wire.
type Parameter interface {
	Name() string
	// In is where the value goes: query, path, header or cookie.
	In() string
	Required() bool
	// List reports whether the parameter takes several values — its schema is an array. How a list
	// is written on the wire is the protocol's: AWS's Query protocol numbers each member, Name.1.
	List() bool
}

// Parameter locations, as OpenAPI names them.
const (
	InQuery  = "query"
	InPath   = "path"
	InHeader = "header"
	InCookie = "cookie"
	// InBody is not an OpenAPI parameter location: a request body is declared as requestBody, not in
	// the parameter list. It is named here because a parser that flattens a body schema into
	// parameters has to say where they go, and a compiler places them by this name like any other.
	InBody = "body"
)

// Schema is the shape a document declares, as much of it as a consumer needs to project a response
// onto it. A document format supplies its own implementation, so nothing consuming this learns how
// the document was written.
//
// It exists because some transforms are schema-driven rather than program-driven: the document names
// one and ships no body, because its instructions ARE the declared shape — the row type and, per
// property, the element name the wire uses.
type Schema interface {
	// Property returns a named property of an object schema.
	Property(name string) (Schema, bool)
	// Properties are the declared property names, sorted.
	Properties() []string
	// Items is the element schema of an array; false where this is not an array.
	Items() (Schema, bool)
	// WireName is the element name the wire uses for this property, empty where the document states
	// none — in which case the property name itself is the element name.
	WireName() string
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
	// Schema is the declared shape of the response, nil where the document states none. A
	// schema-driven transform has no other instructions, so dropping this at the parse boundary is
	// what makes such a transform impossible to run.
	Schema() Schema
	Pagination() Pagination
}

// RequestTransform optionally declares the program that produces an operation's wire body.
type RequestTransform interface{ RequestTransform() Transform }

// Transform is a declared body transformation: a program and the language it is written in.
type Transform interface {
	// Type names the evaluator (e.g. golang_template_mxj_v0.2.0). Empty means none declared.
	Type() string
	Body() string
}

// Pagination is how the document says pages continue. Token keys are paths into the TRANSFORMED
// body, and locations say whether a token travels in the body or the query.
type Pagination interface {
	// Algorithm distinguishes a current page number from an opaque next token.
	Algorithm() string
	// ResponseTerminator is the page-count key and location for page_number.
	ResponseTerminator() (key, location string)
	// RequestToken is the key and location the next-page token is SENT as.
	RequestToken() (key, location string)
	// RequestTokenEncoding is "none" when a query token must be sent verbatim.
	RequestTokenEncoding() string
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

// DefaultProviderPrefix namespaces DOCUMENT-DERIVED providers. A hand-authored catalog entry and a
// document-derived one can describe the same cloud without describing the same thing —
// aws.iam.principals is a curated, normalized view; the document-derived aws.iam_native.users is
// whatever the document says. Colliding them would make an address ambiguous and silently change what
// a caller gets as documents mature, so the namespaces stay separate and the prefix states which
// guarantees apply.
//
// "unstable" is the honest word: these addresses follow the documents, so they can change shape when
// the documents do. A caller that has decided otherwise can set its own prefix.
const DefaultProviderPrefix = "stackql_unstable_"

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
	// Paths are every addressable exchange, sorted. Listing them parses every service's document.
	Paths() []string
	// Address is the published address of one resource of one service — the namespace included —
	// or false where the service's document declares no such resource. Only that document is read.
	Address(service, resource string) (string, bool)
	// Exchange resolves one address to its first SELECT.
	Exchange(path string) (AOTExchange, error)
	// Operations returns every exchange a resource binds to one SQL verb, in the order the document
	// lists them. That order is the selection rule: a verb fans out — a resource may declare several
	// inserts — and the caller's inputs decide which applies, by matching signatures down the list.
	Operations(path, verb string) ([]AOTExchange, error)
}

// Method is one operation a resource declares, and the SQL verb (if any) the document maps it to.
// A method with no verb is reachable by name but not by a SQL statement.
type Method interface {
	Name() string
	// SQLVerb is select/insert/update/delete/exec, or empty when the document maps it to none.
	SQLVerb() string
	OperationID() string
}

// Registry is a directory of PROVIDERS — the shape a tool like stackql already keeps on disk
// (<root>/<provider>/<version>/provider.yaml + services/). It exists so a caller can hand over the
// root and have everything become addressable, rather than naming one bundle at a time.
type Registry interface {
	// Providers are the provider names present, sorted.
	Providers() []string
	// Version is the version resolved for a provider — the newest present, unless one was pinned.
	Version(provider string) (string, bool)
	// Catalog is one provider's bundle.
	Catalog(provider string) (Catalog, error)
	// Exchange resolves a "<provider>.<service>.<resource>" address against whichever provider it names.
	Exchange(address string) (AOTExchange, error)
}

// MethodInputs exposes the SQL presentation names of required method parameters.
type MethodInputs interface{ RequiredParams() []string }

// RequestDefaults carries a fallback body used only when no body is supplied.
type RequestDefaults interface{ BaseBody() string }

// GraphQL describes the request program and continuation declared by an operation.
type GraphQL interface {
	URL() string
	HTTPVerb() string
	Query() string
	Selection() string
	CursorPath() string
	CursorStrategy() string
	CursorFormat() string
	TerminationPath() string
	PageSize() int
}

// GraphQLExchange optionally describes GraphQL acquisition on the ordinary HTTP exchange.
type GraphQLExchange interface{ GraphQL() (GraphQL, bool) }

// ScalarRowSchema names the column of a declared scalar row selection.
// It presents an object schema so discovery and execution share the same shape.
type ScalarRowSchema interface {
	Schema
	ScalarColumn() string
}

// SelectedSchema optionally exposes the row schema reached by a response selection.
type SelectedSchema interface{ RowSchema() Schema }

// ReadControls optionally extends projection/filter/top pushdown with ordering,
// offset and a total-count flag. Existing Pushdown implementations need not change.
type ReadControls interface {
	OrderBy() (PushdownOption, bool)
	Skip() (PushdownOption, bool)
	Count() (PushdownOption, bool)
}

// OrderSyntax describes how a declared ordering option names its terms.
type OrderSyntax interface {
	Syntax() string
	FixedColumn() string
}
