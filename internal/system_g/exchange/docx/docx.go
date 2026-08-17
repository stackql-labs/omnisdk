// Package docx runs plans built from a provider DOCUMENT rather than from hand-authored code. It is
// the compile seam between the two: an AOT description says what a call IS, this says how the engine
// RUNS it.
//
// Spec — the compiler — depends only on the aot CONTRACT, never on a dialect, so a new document
// format is a new parser and no change here. SelectPlan is a convenience over one dialect and is the
// only thing in this package that names stackqldoc; a caller with its own parser uses Spec directly.
//
// The relocation that seam exists for: a document attaches its response program to the SOURCE, while
// a plan applies transforms lazily toward the SINK. So the program becomes a stage in the exchange's
// own pipeline — send → require OK → run the program → decode → explode the item list — and the
// document's objectKey, which addresses the program's OUTPUT, is only meaningful after that stage has
// run. Nothing here knows any provider; everything comes from the document.
package docx

import (
	"fmt"
	"strings"

	"github.com/stackql-labs/omnisdk/internal/system_g/awsv4"
	"github.com/stackql-labs/omnisdk/internal/system_g/bind"
	encoder "github.com/stackql-labs/omnisdk/internal/system_g/endec"
	"github.com/stackql-labs/omnisdk/internal/system_g/exchange"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/httpx"
	"github.com/stackql-labs/omnisdk/internal/system_g/plan"
	"github.com/stackql-labs/omnisdk/internal/system_g/record"
	"github.com/stackql-labs/omnisdk/internal/system_g/transform"
	"github.com/stackql-labs/omnisdk/internal/system_g/value"
	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
	"github.com/stackql-labs/omnisdk/pkg/docparse/dsl"
	"github.com/stackql-labs/omnisdk/pkg/docparse/stackqldoc"
)

// Option configures compilation.
type Option func(*options)

type options struct {
	baseURL  string
	creds    awsv4.Credentials
	signer   facade.Transform
	noSign   bool
	security aot.Security
}

// WithProviderSecurity supplies the scheme the PROVIDER document declares. Auth is stated once for a
// whole provider, and a service document that says nothing is inheriting it, not opting out — so
// without this a signed provider's services would quietly issue unsigned requests.
func WithProviderSecurity(sec aot.Security) Option {
	return func(o *options) { o.security = sec }
}

// WithAWSCredentials supplies the credentials for documents that declare SigV4. The document says a
// call is signed; it cannot say with whose identity, so that stays an explicit caller decision.
func WithAWSCredentials(c awsv4.Credentials) Option {
	return func(o *options) { o.creds = c }
}

// WithRequestTransform overrides the signing the document implies — for a provider whose scheme the
// document describes badly, or a mock that wants the request unsigned.
func WithRequestTransform(t facade.Transform) Option {
	return func(o *options) { o.signer = t }
}

// WithoutSigning drops the implied signing entirely.
func WithoutSigning() Option {
	return func(o *options) { o.noSign = true }
}

// WithBaseURL retargets the document's server, keeping each operation's own path — so a document can
// be run against a mock without editing it. Same idea as an endpoint override for a hand-authored
// plan: where a service lives is config, not part of the call.
func WithBaseURL(base string) Option {
	return func(o *options) { o.baseURL = strings.TrimRight(base, "/") }
}

// SelectPlan builds a runnable plan for a resource's SELECT from provider-document bytes. inputs are
// the κ values the document says the call needs (its server variables, e.g. region) — required and
// never inferred, exactly as for a hand-authored plan.
func SelectPlan(doc []byte, resource string, inputs map[string]any, reg dsl.Registry, opts ...Option) (plan.Plan, error) {
	d, err := stackqldoc.Parse(doc)
	if err != nil {
		return nil, err
	}
	ex, err := d.Select(resource)
	if err != nil {
		return nil, err
	}
	return PlanFor(ex, inputs, reg, opts...)
}

// PlanFor builds a plan from an already-resolved exchange — the path a catalog takes, where the
// address has already done the resolving.
func PlanFor(ex aot.AOTExchange, inputs map[string]any, reg dsl.Registry, opts ...Option) (plan.Plan, error) {
	spec, err := Spec(ex, inputs, reg, opts...)
	if err != nil {
		return nil, err
	}
	for _, in := range ex.Inputs() {
		if v, ok := inputs[in]; !ok || v == "" {
			return nil, fmt.Errorf("docx: exchange %q requires input %q", ex.Name(), in)
		}
	}
	for _, p := range ex.Request().Parameters() {
		if !p.Required() {
			continue
		}
		if v, ok := inputs[p.Name()]; !ok || v == "" {
			return nil, fmt.Errorf("docx: exchange %q requires parameter %q (in %s)",
				ex.Name(), p.Name(), p.In())
		}
	}
	return plan.NewPlan([]plan.ExchangeSpec{spec}, nil, nil, inputs, nil, encoder.NewJSONLEncoder()), nil
}

// Spec compiles one AOT exchange into the engine's exchange declaration. inputs are the values the
// caller supplied: needed HERE, not merely validated later, because which optional parameters exist on
// the wire depends on which were given — an unsupplied optional must not appear as an empty query
// string, and a supplied one must not be silently dropped.
func Spec(ex aot.AOTExchange, inputs map[string]any, reg dsl.Registry, opts ...Option) (plan.ExchangeSpec, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	req, resp := ex.Request(), ex.Response()
	// A transform runs a PROGRAM only when the document ships one. Most declare a type with no body —
	// naming a built-in decoding (golang_template_json_v0.3.0, schema_driven_xml_v0.1.0) rather than
	// supplying code — and for those the body is used as it arrives, decoded by its media type.
	// Requiring an evaluator for a type that carries no program would reject the majority of documents
	// over a program that does not exist.
	tr := resp.Transform()
	hasProgram := tr.Type() != "" && tr.Body() != ""
	if hasProgram {
		if _, ok := reg.Get(tr.Type()); !ok {
			return nil, fmt.Errorf("docx: exchange %q needs evaluator %q (have: %v)", ex.Name(), tr.Type(), reg.Types())
		}
	}
	decode := decoderFor(resp, hasProgram)
	if decode == nil {
		return nil, fmt.Errorf("docx: exchange %q has response media type %q, which is not decodable",
			ex.Name(), resp.MediaType())
	}
	// The document states where the rows are, in one of two syntaxes it uses interchangeably.
	rowPath := parseObjectKey(resp.ObjectKey())

	url := retarget(req.URL(), o.baseURL)
	hreq := httpx.Request{Method: req.Method(), URL: url}
	if params := req.Params(); len(params) > 0 {
		body := make(map[string]any, len(params))
		for k, v := range params {
			body[k] = v
		}
		hreq.Body = httpx.Body{Encoding: encodingOf(req.MediaType()), Params: body}
	}

	// Place each declared parameter where the DOCUMENT says it belongs. Every string in an
	// httpx.Request is a {name} template resolved from the bound row, so placing a parameter is
	// declaring where its template goes — and binding its name so a value reaches it.
	bindings := ex.Inputs()
	for _, p := range req.Parameters() {
		_, supplied := inputs[p.Name()]
		if !supplied && !p.Required() {
			continue // an optional parameter nobody supplied is absent, not empty
		}
		tmpl := "{" + p.Name() + "}"
		switch p.In() {
		case aot.InQuery:
			if hreq.Query == nil {
				hreq.Query = map[string]string{}
			}
			hreq.Query[p.Name()] = tmpl
		case aot.InHeader:
			if hreq.Headers == nil {
				hreq.Headers = map[string]string{}
			}
			hreq.Headers[p.Name()] = tmpl
		case aot.InPath:
			// already templated into the URL by the document; it only needs binding
		default:
			continue // a location we do not place must not be bound as if we had
		}
		bindings = withInput(bindings, p.Name())
	}
	// A {placeholder} in the URL that no parameter declared still has to be bound, or it resolves to
	// nothing and the request goes out with an empty path segment.
	for _, name := range placeholders(url) {
		if _, supplied := inputs[name]; supplied {
			bindings = withInput(bindings, name)
		}
	}

	// Signing is IMPLICIT: the document declares the scheme for every call it describes, so requiring
	// a caller to restate it per exchange is how a request goes out unsigned. Overridable, never
	// silently skipped — a declared scheme with no way to satisfy it is an error, not a plain request.
	service := serviceOf(req.URL())
	sec := effectiveSecurity(ex.Security(), o.security)
	if sec.Scheme() == aot.SchemeAWSSigV4 && !o.noSign && o.signer == nil && o.creds.AccessKeyID == "" {
		return nil, fmt.Errorf("docx: exchange %q declares %s (%q) but no credentials were supplied",
			ex.Name(), sec.Scheme(), sec.Name())
	}

	// SigV4 always signs into a region, but a GLOBAL service (iam.amazonaws.com) declares no {region}
	// server variable — so nothing would bind one and the signature would be scoped to "". Signing
	// needs it whether or not the URL does, so it joins the inputs explicitly.
	if sec.Scheme() == aot.SchemeAWSSigV4 && !o.noSign && o.signer == nil {
		bindings = withInput(bindings, "region")
	}

	return plan.NewExchangeSpec(ex.Name(), bindings, nil, func(bound map[string]any) facade.Operator {
		var reqT []facade.Transform
		switch {
		case o.noSign:
		case o.signer != nil:
			reqT = append(reqT, o.signer)
		case sec.Scheme() == aot.SchemeAWSSigV4:
			// region is a bound input, so the signer is built per run, not per plan
			reqT = append(reqT, awsv4.NewSigV4Transform(
				awsv4.NewSigV4Signer(str(bound["region"]), service, o.creds, false)))
		}
		send := httpx.Make(hreq, nil, reqT...)(bound)
		// non-2xx fails loudly rather than looking like an empty result
		checked := exchange.NewTransformExchange(0, send, httpx.NewRequireOK(), 1)
		// the document's own program, moved from source to here — when it declares one
		body := checked
		if hasProgram {
			body = exchange.NewTransformExchange(0, checked, program(reg, tr.Type(), tr.Body()), 1)
		}
		decoded := exchange.NewTransformExchange(0, body, decode, 1)
		listed := exchange.NewTransformExchange(0, decoded, itemsAt(rowPath), 1)
		return exchange.NewExplodeRows(listed, 1)
		// INNER, not left-outer: this is a SELECT, and an empty result set is zero rows. A left-outer
		// would emit one row of bare inputs, which reads as "one instance with no fields".
	}, bind.NewInnerFlatten()), nil
}

// program runs a declared body program over the raw response, replacing the payload with its output.
// It is the only place a document's embedded language touches the engine.
func program(reg dsl.Registry, typ, body string) facade.Transform {
	return programTransform{reg: reg, typ: typ, body: body}
}

type programTransform struct {
	reg  dsl.Registry
	typ  string
	body string
}

func (t programTransform) Apply(in facade.Page) (facade.Record, error) {
	out, err := t.reg.Eval(t.typ, t.body, in.Bytes(facade.AnonymousPayload))
	if err != nil {
		return nil, err
	}
	return record.NewRecord(map[string]facade.Value{
		facade.AnonymousPayload: value.NewBytesValue(out),
	}), nil
}

// parseObjectKey reads a declared objectKey into steps. The corpus uses two syntaxes and they are
// distinguishable by their first character: "$" is JSONPath ($.line_items), "/" is XPath
// (/*/vpcSet/item), and empty means the root. "*" is the XPath wildcard for the single root element.
func parseObjectKey(key string) []string {
	k := strings.TrimSpace(key)
	switch {
	case k == "":
		return nil
	case strings.HasPrefix(k, "$"):
		return splitNonEmpty(strings.TrimPrefix(k, "$"), ".")
	default:
		return splitNonEmpty(k, "/")
	}
}

func splitNonEmpty(s, sep string) []string {
	var out []string
	for _, p := range strings.Split(s, sep) {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// itemsAt lifts the document's rows out of the program's output. It selects no columns: the
// document's program has ALREADY decided each item's shape, so imposing a column list would silently
// drop fields it chose to emit. Cardinality is whatever the path lands on — an array is many rows, an
// object is one. That too is the document's statement, not a guess: /*/vpcSet/item names a repeated
// element, /*/GetAccountSummaryResult names a single one.
func itemsAt(steps []string) facade.Transform { return items{steps: steps} }

type items struct{ steps []string }

func (t items) Apply(in facade.Page) (facade.Record, error) {
	doc, ok := in.Doc(facade.AnonymousPayload)
	if !ok {
		return nil, fmt.Errorf("docx: transformed body is not an agnostic document")
	}
	cur := doc
	for _, step := range t.steps {
		if cur == nil {
			break
		}
		m, isMap := cur.(map[string]any)
		if !isMap {
			cur = nil
			break
		}
		if step == "*" {
			// The XPath wildcard addresses the single root element, which is what an XML document
			// always has exactly one of.
			if len(m) != 1 {
				cur = nil
				break
			}
			for _, v := range m {
				cur = v
			}
			continue
		}
		cur = m[step]
	}
	switch v := cur.(type) {
	case []any:
		return docRecord(v), nil
	case map[string]any:
		return docRecord([]any{v}), nil // a single element is one row
	default:
		return docRecord([]any{}), nil // absent is an answer, not a failure
	}
}

func docRecord(list []any) facade.Record {
	return record.NewRecord(map[string]facade.Value{facade.AnonymousPayload: value.NewDocValue(list)})
}

// decoderFor picks the body decoder. A declared transform states what it turns the body INTO
// (overrideMediaType); without one, the body arrives as the wire media type says.
func decoderFor(resp aot.Response, hasProgram bool) facade.Transform {
	// Without a program the body arrives as the wire says; with one, overrideMediaType states what the
	// program turned it into.
	mt := resp.MediaType()
	if hasProgram && resp.OverrideMediaType() != "" {
		mt = resp.OverrideMediaType()
	}
	switch {
	case strings.Contains(mt, "xml"):
		return transform.NewXMLToAgnostic()
	case strings.Contains(mt, "json"):
		return transform.NewJSONToAgnostic()
	default:
		return nil
	}
}

// effectiveSecurity prefers what the operation itself declares and falls back to the provider's.
// Silence at the service level means inheritance, never "none".
func effectiveSecurity(operation, provider aot.Security) aot.Security {
	if operation != nil && operation.Scheme() != aot.SchemeNone {
		return operation
	}
	if provider != nil {
		return provider
	}
	return operation
}

// serviceOf is the AWS service a host names (ec2.{region}.amazonaws.com → "ec2"), which SigV4 signs
// into the credential scope. Taken from the document's server rather than configured, so it cannot
// disagree with the host being called.
func serviceOf(rawURL string) string {
	i := strings.Index(rawURL, "://")
	if i < 0 {
		return ""
	}
	host := rawURL[i+3:]
	if j := strings.IndexAny(host, "/?"); j >= 0 {
		host = host[:j]
	}
	label, _, _ := strings.Cut(host, ".")
	return label
}

// placeholders are the {name} templates in a URL.
func placeholders(url string) []string {
	var out []string
	rest := url
	for {
		i := strings.Index(rest, "{")
		if i < 0 {
			return out
		}
		rest = rest[i+1:]
		j := strings.Index(rest, "}")
		if j < 0 {
			return out
		}
		out = append(out, rest[:j])
		rest = rest[j+1:]
	}
}

// withInput adds name to inputs if absent, preserving order.
func withInput(inputs []string, name string) []string {
	for _, in := range inputs {
		if in == name {
			return inputs
		}
	}
	return append(append([]string{}, inputs...), name)
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// retarget swaps a URL's scheme://host for base, keeping the path. Empty base leaves it alone.
func retarget(rawURL, base string) string {
	if base == "" {
		return rawURL
	}
	i := strings.Index(rawURL, "://")
	if i < 0 {
		return base
	}
	rest := rawURL[i+3:]
	if j := strings.Index(rest, "/"); j >= 0 {
		return base + rest[j:]
	}
	return base
}

func encodingOf(mediaType string) httpx.Encoding {
	switch {
	case strings.Contains(mediaType, "x-www-form-urlencoded"):
		return httpx.EncodingForm
	case strings.Contains(mediaType, "json"):
		return httpx.EncodingJSON
	default:
		return httpx.EncodingNone
	}
}
