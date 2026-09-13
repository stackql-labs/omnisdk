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
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/stackql-labs/omnisdk/internal/system_g/awsv4"
	"github.com/stackql-labs/omnisdk/internal/system_g/bind"
	encoder "github.com/stackql-labs/omnisdk/internal/system_g/endec"
	"github.com/stackql-labs/omnisdk/internal/system_g/exchange"
	"github.com/stackql-labs/omnisdk/internal/system_g/exchange/sdk"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/httpx"
	"github.com/stackql-labs/omnisdk/internal/system_g/plan"
	"github.com/stackql-labs/omnisdk/internal/system_g/record"
	"github.com/stackql-labs/omnisdk/internal/system_g/transform"
	"github.com/stackql-labs/omnisdk/internal/system_g/value"
	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
	"github.com/stackql-labs/omnisdk/pkg/docparse/dsl"
	"github.com/stackql-labs/omnisdk/pkg/docparse/dsl/schemaxml"
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
	gcpCreds *sdk.GCPCredentials
	// ignoreResponse compiles an operation whose response the document does not type.
	ignoreResponse bool
	// wholeResponse emits the decoded body as one record rather than exploding a list out of it.
	wholeResponse bool
	// bound names parameters that arrive over a β edge rather than from the caller.
	bound map[string]bool
	// provided names parameters T_in builds. They are PLACED on the wire but not bound: no producer
	// emits them, so declaring them as inputs would leave the plan looking for a β source that by
	// definition does not exist.
	provided map[string]bool
	// inbox names values T_in CONSUMES. They are bound — a producer emits each one — but never
	// placed: they are raw material for the transform, not parameters the service has heard of.
	inbox map[string]bool
	// Response overrides: a document can be wrong for this engine — declaring a row path that only
	// exists after a reshape nobody implements, or a media type the service does not actually send —
	// and editing the bundle is not the remedy. A caller states what the document should have said.
	objectKey        string
	mediaType        string
	programType      string
	programBody      string
	overrideResponse bool
}

// MetadataKey is where a response's own report about itself sits within the response document:
// the status it returned, and whatever else the call said about itself rather than about the object.
//
// It is placed at the RESPONSE level, above any item list. A SELECT's rows are items exploded out
// of the body, and stamping the status onto each one would attach a property of the reply to things
// inside it. So metadata is reachable on the whole response and elided from the rows — never
// discarded, which is the difference between a flow choosing not to show something and it being
// unavailable.
const MetadataKey = "_response"

// withMetadata is the response as one value: what the call reported about itself, beside what it
// returned. A body the document does not type is the degenerate case — metadata and nothing else —
// rather than a different shape.
type withMetadata struct{ body facade.Transform }

func (m withMetadata) Apply(in facade.Page) (facade.Record, error) {
	meta := map[string]any{httpx.KeyStatus: string(in.Bytes(httpx.KeyStatus))}
	doc := map[string]any{MetadataKey: meta}

	if m.body != nil {
		decoded, err := m.body.Apply(in)
		if err != nil {
			return nil, err
		}
		if payload, ok := decoded.Doc(facade.AnonymousPayload); ok {
			if fields, ok := payload.(map[string]any); ok {
				for k, v := range fields {
					// The body wins a name clash: a provider that genuinely returns a field called
					// _response means its own, and shadowing it would be inventing data.
					doc[k] = v
				}
			} else {
				doc[facade.AnonymousPayload] = payload
			}
		}
	}
	return record.NewRecord(map[string]facade.Value{
		facade.AnonymousPayload: value.NewDocValue(doc),
	}), nil
}

// WithGoogleCredentials supplies the service-account key for documents that declare service_account
// auth. As with SigV4, the document says a call is authenticated; whose identity it uses stays an
// explicit caller decision.
func WithGoogleCredentials(c sdk.GCPCredentials) Option {
	return func(o *options) { o.gcpCreds = &c }
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

// WithoutResponseDecode tolerates a response the document does not type. It exists for mutating
// operations: a delete's reply is frequently undocumented, and a plan that cannot be built because
// of that is a plan that cannot undo anything.
func WithoutResponseDecode() Option {
	return func(o *options) { o.ignoreResponse = true }
}

// WithProvided declares parameters the consumer's inbound transform builds. They are placed on the
// request so the value has somewhere to go, and deliberately not bound: T_in makes them out of the
// inbox, so there is no producer emitting them by name.
func WithProvided(names ...string) Option {
	return func(o *options) {
		if o.provided == nil {
			o.provided = map[string]bool{}
		}
		for _, n := range names {
			o.provided[n] = true
		}
	}
}

// WithInbox declares values the consumer's inbound transform consumes. They are bound so a producer
// can deliver them, and deliberately not placed: the service knows nothing about them, and sending
// one would be an unrecognised parameter.
func WithInbox(names ...string) Option {
	return func(o *options) {
		if o.inbox == nil {
			o.inbox = map[string]bool{}
		}
		for _, n := range names {
			o.inbox[n] = true
		}
	}
}

// WithObjectKey overrides the document's declared row path: where in the decoded body the item list
// lives. A document that names a path only produced by a transform nobody implements yields no rows
// and says nothing about why, and this is how a caller corrects it without editing the bundle.
func WithObjectKey(key string) Option {
	return func(o *options) { o.objectKey, o.overrideResponse = key, true }
}

// WithMediaType overrides what the document says the response is, for a service that sends
// something else.
func WithMediaType(mt string) Option {
	return func(o *options) { o.mediaType, o.overrideResponse = mt, true }
}

// WithResponseProgram overrides the document's response transform with one the caller supplies.
func WithResponseProgram(typ, body string) Option {
	return func(o *options) { o.programType, o.programBody, o.overrideResponse = typ, body, true }
}

// WithBound declares parameters that will arrive over a β edge rather than being supplied up front.
//
// Placement otherwise depends on what the caller gave: an optional parameter nobody supplied is
// absent rather than empty, which is right for a lone call and wrong for a joined one. A value
// bound from another exchange is not known when the plan is built, and without this the parameter
// is dropped and the edge has nothing to bind to.
func WithBound(names ...string) Option {
	return func(o *options) {
		if o.bound == nil {
			o.bound = map[string]bool{}
		}
		for _, n := range names {
			o.bound[n] = true
		}
	}
}

// WithWholeResponse emits the decoded body as a single record instead of exploding the item list
// the document's objectKey names. It is for mutating operations, which answer about one object: the
// objectKey describes where a SELECT finds its items, and a create's reply has no such list.
func WithWholeResponse() Option {
	return func(o *options) { o.wholeResponse = true }
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
	specs := []plan.ExchangeSpec{spec}
	var betas []plan.BetaEdge
	if c, ok := spec.(compiled); ok && c.auth != nil {
		// The token exchange runs FIRST and its {token} flows to the call, exactly as a hand-authored
		// plan wires it.
		specs = []plan.ExchangeSpec{c.auth, c.spec}
		betas = append(betas, plan.NewBetaEdge(c.auth.Name(), c.spec.Name(), "token", "token"))
		inputs["assertion"] = c.assertion
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
	// Auth κ inputs are plumbing, not data: a signed assertion and a bearer token travel on the row so
	// the exchanges can bind them, and emitting them would put a live credential in every result.
	return plan.NewPlan(specs, betas, nil, inputs,
		[]facade.Transform{dropKeys{"assertion", "token"}}, encoder.NewJSONLEncoder()), nil
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
	resp = overridden(resp, o)
	tr := resp.Transform()
	// A transform with a type and no body is not "no transform": it names an evaluator whose
	// instructions are the document's own schema. Treating it as absent is what made a
	// schema-driven document resolve its rows at a path only that transform produces.
	hasProgram := tr.Type() != "" && (tr.Body() != "" || schemaDriven(reg, tr.Type()))
	if hasProgram {
		if _, ok := reg.Get(tr.Type()); !ok {
			return nil, fmt.Errorf("docx: exchange %q needs evaluator %q (have: %v)", ex.Name(), tr.Type(), reg.Types())
		}
	}
	decode := decoderFor(resp, hasProgram)
	if o.wholeResponse {
		// The whole response is the value: metadata beside the body, with no item list exploded out
		// of it, so nothing the call reported is left unreachable.
		decode = withMetadata{body: decode}
	} else if decode == nil && o.ignoreResponse {
		// A mutating call whose response the document does not type: the effect is the point and the
		// body carries nothing the caller reads. Refusing to compile it would make a delete
		// unreachable because of how its reply was documented. The response still arrives — as
		// metadata with an empty body.
		decode = withMetadata{}
	}
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
	placed := map[string]bool{}
	for _, p := range req.Parameters() {
		_, supplied := inputs[p.Name()]
		if !supplied && !p.Required() && !o.bound[p.Name()] && !o.provided[p.Name()] {
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
		placed[p.Name()] = true
		if o.provided[p.Name()] {
			// Placed, not bound: T_in builds it from the inbox, so no producer emits it.
			continue
		}
		bindings = withInput(bindings, p.Name())
	}
	// A provided name the document does not declare is placed anyway. The caller is asserting the
	// wire shape, exactly as an override asserts the response shape: AWS's Query API takes a filter
	// as Filter.1.Name / Filter.1.Value.1, while the document models it as one "Filter" parameter of
	// a list type it never says how to serialise. Refusing to send what the document did not name
	// would make the call unreachable over a modelling gap.
	for name := range o.provided {
		if _, declared := placed[name]; declared {
			continue
		}
		if hreq.Query == nil {
			hreq.Query = map[string]string{}
		}
		hreq.Query[name] = "{" + name + "}"
	}
	// Raw material for T_in: bound so a producer delivers it, never placed on the wire.
	for name := range o.inbox {
		bindings = withInput(bindings, name)
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
	if !o.noSign && o.signer == nil {
		switch sec.Scheme() {
		case aot.SchemeAWSSigV4:
			if o.creds.AccessKeyID == "" {
				return nil, fmt.Errorf("docx: exchange %q declares %s (%q) but no credentials were supplied",
					ex.Name(), sec.Scheme(), sec.Name())
			}
		case aot.SchemeServiceAccount:
			if o.gcpCreds == nil {
				return nil, fmt.Errorf("docx: exchange %q declares %s (%q) but no credentials were supplied",
					ex.Name(), sec.Scheme(), sec.Name())
			}
		}
	}

	// A service account authenticates by TOKEN EXCHANGE, not by signing the request: the key buys an
	// access token, which every call then carries. That is an extra exchange in the plan rather than a
	// request transform, which is why it is built here and not alongside the signer.
	var authSpec plan.ExchangeSpec
	var assertion string
	if sec.Scheme() == aot.SchemeServiceAccount && o.gcpCreds != nil && !o.noSign {
		authSpec, assertion = sdk.GCPOAuthSpec(o.baseURL, *o.gcpCreds, googleScope)
		if hreq.Headers == nil {
			hreq.Headers = map[string]string{}
		}
		hreq.Headers["Authorization"] = "Bearer {token}"
		bindings = withInput(bindings, "token")
	}

	// SigV4 always signs into a region, but a GLOBAL service (iam.amazonaws.com) declares no {region}
	// server variable — so nothing would bind one and the signature would be scoped to "". Signing
	// needs it whether or not the URL does, so it joins the inputs explicitly.
	if sec.Scheme() == aot.SchemeAWSSigV4 && !o.noSign && o.signer == nil {
		bindings = withInput(bindings, "region")
	}

	spec := plan.NewExchangeSpec(ex.Name(), bindings, nil, func(bound map[string]any) facade.Operator {
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
			body = exchange.NewTransformExchange(0, checked, program(reg, tr.Type(), tr.Body(), dslContext(resp, rowPath)), 1)
		}
		decoded := exchange.NewTransformExchange(0, body, decode, 1)
		if o.wholeResponse {
			// A mutating call answers about ONE object, not a list. The document's objectKey
			// describes where a SELECT finds its items, and applying it here explodes a reply that
			// has no list — yielding zero rows for a call that plainly succeeded.
			return decoded
		}
		listed := exchange.NewTransformExchange(0, decoded, itemsAt(rowPath), 1)
		return exchange.NewExplodeRows(listed, 1)
		// INNER, not left-outer: this is a SELECT, and an empty result set is zero rows. A left-outer
		// would emit one row of bare inputs, which reads as "one instance with no fields".
	}, bind.NewInnerFlatten())
	if authSpec != nil {
		return compiled{spec: spec, auth: authSpec, assertion: assertion}, nil
	}
	return compiled{spec: spec}, nil
}

// compiled carries an exchange plus the auth exchange it depends on, so PlanFor can wire both into
// one plan. A token exchange is part of the plan, not of the request.
type compiled struct {
	spec      plan.ExchangeSpec
	auth      plan.ExchangeSpec
	assertion string
}

func (c compiled) Name() string                              { return c.spec.Name() }
func (c compiled) In() []string                              { return c.spec.In() }
func (c compiled) Out() []string                             { return c.spec.Out() }
func (c compiled) Make(bound map[string]any) facade.Operator { return c.spec.Make(bound) }
func (c compiled) Flatten() facade.Transform                 { return c.spec.Flatten() }
func (c compiled) Inbound() facade.Transform                 { return c.spec.Inbound() }

// dropKeys removes named attributes on the way out.
type dropKeys []string

func (d dropKeys) Apply(in facade.Page) (facade.Record, error) {
	doc, ok := in.Doc(facade.AnonymousPayload)
	if !ok {
		return nil, nil
	}
	m, ok := doc.(map[string]any)
	if !ok {
		return record.NewRecord(map[string]facade.Value{facade.AnonymousPayload: value.NewDocValue(doc)}), nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	for _, k := range d {
		delete(out, k)
	}
	return record.NewRecord(map[string]facade.Value{facade.AnonymousPayload: value.NewDocValue(out)}), nil
}

// googleScope is the read scope a document-compiled Google call requests. The documents do not state
// one, and cloud-platform.read-only is the least that serves a SELECT.
// googleScope is what the token exchange asks for. cloud-platform.read-only reads most APIs but
// Compute rejects it outright — ACCESS_TOKEN_SCOPE_INSUFFICIENT, whatever roles the identity holds —
// so the broad scope is requested and the identity's own roles remain the limit on what it can do.
//
// The right answer is the scopes the operation declares; every Compute method names its own. Until
// those are carried through the parse boundary, this is the one that works everywhere.
const googleScope = "https://www.googleapis.com/auth/cloud-platform"

// program runs a declared body program over the raw response, replacing the payload with its output.
// It is the only place a document's embedded language touches the engine.
func program(reg dsl.Registry, typ, body string, ctx dsl.Context) facade.Transform {
	return programTransform{reg: reg, typ: typ, body: body, ctx: ctx}
}

// schemaDriven reports whether a registered evaluator takes its instructions from the schema rather
// than from a program body. A document naming one ships no text, so the usual "type and body" test
// reads it as absent.
func schemaDriven(reg dsl.Registry, typ string) bool {
	return typ == schemaxml.Type && func() bool { _, ok := reg.Get(typ); return ok }()
}

// dslContext is what a schema-driven transform needs and a program-driven one ignores: the declared
// response shape, and the envelope key the document's row path then points at.
func dslContext(resp aot.Response, rowPath []string) dsl.Context {
	ctx := dsl.Context{Schema: resp.Schema()}
	if len(rowPath) == 1 {
		ctx.ListProperty = rowPath[0]
	}
	return ctx
}

type programTransform struct {
	reg  dsl.Registry
	typ  string
	body string
	ctx  dsl.Context
}

func (t programTransform) Apply(in facade.Page) (facade.Record, error) {
	out, err := t.reg.Eval(t.typ, t.ctx, t.body, in.Bytes(facade.AnonymousPayload))
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
// overridden applies the caller's corrections to what the document said about the response.
func overridden(resp aot.Response, o options) aot.Response {
	if !o.overrideResponse {
		return resp
	}
	out := responseOverride{
		Response:  resp,
		objectKey: resp.ObjectKey(),
		mediaType: resp.MediaType(),
		transform: resp.Transform(),
	}
	if o.objectKey != "" {
		out.objectKey = o.objectKey
	}
	if o.mediaType != "" {
		out.mediaType = o.mediaType
	}
	if o.programType != "" {
		out.transform = programOverride{typ: o.programType, body: o.programBody}
	}
	return out
}

type responseOverride struct {
	aot.Response
	objectKey string
	mediaType string
	transform aot.Transform
}

func (r responseOverride) ObjectKey() string        { return r.objectKey }
func (r responseOverride) MediaType() string        { return r.mediaType }
func (r responseOverride) Transform() aot.Transform { return r.transform }

// OverrideMediaType is cleared: the document's override describes what ITS transform produced, and a
// caller who has replaced that has said what the body actually is.
func (r responseOverride) OverrideMediaType() string { return "" }

type programOverride struct{ typ, body string }

func (p programOverride) Type() string { return p.typ }
func (p programOverride) Body() string { return p.body }

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

// InboundProgram compiles a DSL program as T_in: the assembled inbox goes in, the consumer's inputs
// come out.
//
// It is the same language a document's response transforms are written in, pointed the other way.
// The inbox is handed to the program as a JSON object and its output is read back as one, so a
// caller shapes inbound values exactly as a document shapes outbound ones.
func InboundProgram(reg dsl.Registry, typ, body string) facade.Transform {
	return inboundProgram{reg: reg, typ: typ, body: body}
}

type inboundProgram struct {
	reg  dsl.Registry
	typ  string
	body string
}

func (p inboundProgram) Apply(in facade.Page) (facade.Record, error) {
	inbox, ok := bind.DocMap(in)
	if !ok {
		return nil, fmt.Errorf("docx: inbound transform received no inbox")
	}
	raw, err := json.Marshal(inbox)
	if err != nil {
		return nil, fmt.Errorf("docx: encode inbox: %w", err)
	}
	out, err := p.reg.Eval(p.typ, dsl.Context{}, p.body, raw)
	if err != nil {
		return nil, fmt.Errorf("docx: inbound program: %w", err)
	}
	// Numbers keep their written form: an identifier that survives the wire as digits must not be
	// rewritten by a float64 on its way into the next call.
	dec := json.NewDecoder(bytes.NewReader(out))
	dec.UseNumber()
	var shaped map[string]any
	if err := dec.Decode(&shaped); err != nil {
		return nil, fmt.Errorf("docx: inbound program produced %q, which is not a JSON object: %w", out, err)
	}
	return bind.NewDocRecord(shaped), nil
}

// Expand unpacks what a compiled spec needs beside itself.
//
// A document that declares service-account auth compiles to TWO exchanges: a token exchange and the
// call, joined by a β edge carrying the bearer. PlanFor does that wiring for a single-exchange plan;
// a caller composing several exchanges into one plan has to do it per exchange, or every call goes
// out unauthenticated — or, as happens here, the plan refuses to build because nothing supplies the
// token.
//
// ok is false for a spec that needs nothing extra, which is the common case.
// The caller builds the edge itself, because it may rename either side first: a plan names its
// exchanges, and composing several documents means giving each a distinct name.
func Expand(spec plan.ExchangeSpec) (auth plan.ExchangeSpec, assertion string, ok bool) {
	c, is := spec.(compiled)
	if !is || c.auth == nil {
		return nil, "", false
	}
	return c.auth, c.assertion, true
}

// TokenAttr is the attribute an auth exchange emits and the call it feeds binds.
const TokenAttr = "token"

// Rename returns the spec under a different name.
//
// A plan names its exchanges, and two documents routinely name a method the same thing — "list" is
// the obvious one. Composing them into one plan needs the names distinct, and the address is what
// distinguishes them.
func Rename(spec plan.ExchangeSpec, name string) plan.ExchangeSpec {
	return renamed{ExchangeSpec: spec, name: name}
}

type renamed struct {
	plan.ExchangeSpec
	name string
}

func (r renamed) Name() string { return r.name }
