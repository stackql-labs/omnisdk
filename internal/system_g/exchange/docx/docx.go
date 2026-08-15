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
	baseURL string
	creds   awsv4.Credentials
	signer  facade.Transform
	noSign  bool
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
	spec, err := Spec(ex, reg, opts...)
	if err != nil {
		return nil, err
	}
	for _, in := range ex.Inputs() {
		if v, ok := inputs[in]; !ok || v == "" {
			return nil, fmt.Errorf("docx: resource %q requires input %q", resource, in)
		}
	}
	return plan.NewPlan([]plan.ExchangeSpec{spec}, nil, nil, inputs, nil, encoder.NewJSONLEncoder()), nil
}

// Spec compiles one AOT exchange into the engine's exchange declaration.
func Spec(ex aot.AOTExchange, reg dsl.Registry, opts ...Option) (plan.ExchangeSpec, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	req, resp := ex.Request(), ex.Response()
	tr := resp.Transform()
	if tr.Type() == "" {
		return nil, fmt.Errorf("docx: exchange %q declares no response transform", ex.Name())
	}
	if _, ok := reg.Get(tr.Type()); !ok {
		return nil, fmt.Errorf("docx: exchange %q needs evaluator %q (have: %v)", ex.Name(), tr.Type(), reg.Types())
	}
	rowPath := itemPath(resp.ObjectKey())
	if rowPath == "" {
		return nil, fmt.Errorf("docx: exchange %q declares no objectKey", ex.Name())
	}

	url := retarget(req.URL(), o.baseURL)
	hreq := httpx.Request{Method: req.Method(), URL: url}
	if params := req.Params(); len(params) > 0 {
		body := make(map[string]any, len(params))
		for k, v := range params {
			body[k] = v
		}
		hreq.Body = httpx.Body{Encoding: encodingOf(req.MediaType()), Params: body}
	}

	// Signing is IMPLICIT: the document declares the scheme for every call it describes, so requiring
	// a caller to restate it per exchange is how a request goes out unsigned. Overridable, never
	// silently skipped — a declared scheme with no way to satisfy it is an error, not a plain request.
	service := serviceOf(req.URL())
	sec := ex.Security()
	if sec.Scheme() == aot.SchemeAWSSigV4 && !o.noSign && o.signer == nil && o.creds.AccessKeyID == "" {
		return nil, fmt.Errorf("docx: exchange %q declares %s (%q) but no credentials were supplied",
			ex.Name(), sec.Scheme(), sec.Name())
	}

	return plan.NewExchangeSpec(ex.Name(), ex.Inputs(), nil, func(bound map[string]any) facade.Operator {
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
		// the document's own program, moved from source to here
		shaped := exchange.NewTransformExchange(0, checked, program(reg, tr.Type(), tr.Body()), 1)
		decoded := exchange.NewTransformExchange(0, shaped, transform.NewJSONToAgnostic(), 1)
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

// itemsAt lifts the document's item list out of the program's output. Unlike a column projection it
// selects nothing: the document's program has ALREADY decided each item's shape, so imposing a column
// list here would silently drop fields it chose to emit.
func itemsAt(path string) facade.Transform { return items{path: path} }

type items struct{ path string }

func (t items) Apply(in facade.Page) (facade.Record, error) {
	doc, ok := in.Doc(facade.AnonymousPayload)
	if !ok {
		return nil, fmt.Errorf("docx: transformed body is not an agnostic document")
	}
	m, ok := doc.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("docx: transformed body is not a map")
	}
	list, ok := m[t.path].([]any)
	if !ok {
		// A page with no items is an answer, not a failure.
		return record.NewRecord(map[string]facade.Value{
			facade.AnonymousPayload: value.NewDocValue([]any{}),
		}), nil
	}
	return record.NewRecord(map[string]facade.Value{
		facade.AnonymousPayload: value.NewDocValue(list),
	}), nil
}

// itemPath strips the JSONPath root from a declared objectKey ("$.line_items" → "line_items").
func itemPath(objectKey string) string {
	return strings.TrimPrefix(strings.TrimPrefix(objectKey, "$"), ".")
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
