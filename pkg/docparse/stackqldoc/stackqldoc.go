// Package stackqldoc parses a stackql provider document (OpenAPI plus the x-stackQL-resources
// extension) into exchanges the engine can run. Input is document BYTES; output is a collection of
// exchanges. Nothing here reaches back into the engine's catalog — a document is data, and this
// package is the only thing that knows its shape.
//
// The document already states, declaratively, everything an exchange needs: which operation backs a
// resource's SELECT, which path and verb that operation is, the server URL and its variables, and how
// pages continue. So parsing is a resolution problem — follow the document's own $refs — not a
// translation problem, and the result is an Exchange built from the same finite request vocabulary
// hand-authored exchanges use.
package stackqldoc

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/httpx"
	"github.com/stackql-labs/omnisdk/internal/system_g/transform"
)

// Exchange is one runnable operation parsed out of a document. The method set is deliberately the
// engine's exchange declaration, so a parsed exchange can be handed straight to the planner without
// this package importing the planner or the planner knowing documents exist.
type Exchange interface {
	// Name identifies the exchange within a plan (e.g. "instances").
	Name() string
	// In are the inputs it binds — server variables and required parameters (e.g. "region").
	In() []string
	// Out are the attributes it publishes to downstream exchanges.
	Out() []string
	// Make builds the operator for a bound input row.
	Make(bound map[string]any) facade.Operator
	// Flatten is how its output merges into the running row; nil means the generic merge.
	Flatten() facade.Transform
}

// Doc is a parsed provider document.
type Doc interface {
	// Resources are the resource names the document declares, sorted.
	Resources() []string
	// Select returns the exchange backing a resource's SELECT verb — the "list this thing" operation.
	// It is an error if the resource has no SELECT, which is a real answer about the provider rather
	// than an empty result.
	Select(resource string) (Exchange, error)
}

// Parse reads a stackql provider document.
func Parse(b []byte) (Doc, error) {
	var d document
	if err := yaml.Unmarshal(b, &d); err != nil {
		return nil, fmt.Errorf("stackqldoc: parse: %w", err)
	}
	if len(d.Components.Resources) == 0 {
		return nil, fmt.Errorf("stackqldoc: no x-stackQL-resources in document")
	}
	return &d, nil
}

// ---- document shape (only the parts an exchange needs) ----------------------

type document struct {
	Servers    []server                     `yaml:"servers"`
	Paths      map[string]map[string]pathOp `yaml:"paths"`
	Components struct {
		Resources map[string]resource `yaml:"x-stackQL-resources"`
	} `yaml:"components"`
}

type server struct {
	URL       string                    `yaml:"url"`
	Variables map[string]serverVariable `yaml:"variables"`
}

type serverVariable struct {
	Default string `yaml:"default"`
}

type resource struct {
	ID       string            `yaml:"id"`
	Title    string            `yaml:"title"`
	Methods  map[string]method `yaml:"methods"`
	SQLVerbs map[string][]ref  `yaml:"sqlVerbs"`
}

type ref struct {
	Ref string `yaml:"$ref"`
}

type method struct {
	Operation ref `yaml:"operation"`
	Config    struct {
		Pagination struct {
			RequestToken  tokenSpec `yaml:"requestToken"`
			ResponseToken tokenSpec `yaml:"responseToken"`
		} `yaml:"pagination"`
	} `yaml:"config"`
	Request struct {
		MediaType string `yaml:"mediaType"`
	} `yaml:"request"`
	Response struct {
		MediaType         string `yaml:"mediaType"`
		OverrideMediaType string `yaml:"overrideMediaType"`
		ObjectKey         string `yaml:"objectKey"`
	} `yaml:"response"`
}

type tokenSpec struct {
	Key      string `yaml:"key"`
	Location string `yaml:"location"`
}

type pathOp struct {
	OperationID string      `yaml:"operationId"`
	Parameters  []parameter `yaml:"parameters"`
}

type parameter struct {
	Name     string `yaml:"name"`
	In       string `yaml:"in"`
	Required bool   `yaml:"required"`
}

// ---- resolution -------------------------------------------------------------

func (d *document) Resources() []string {
	out := make([]string, 0, len(d.Components.Resources))
	for name := range d.Components.Resources {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func (d *document) Select(name string) (Exchange, error) {
	res, ok := d.Components.Resources[name]
	if !ok {
		return nil, fmt.Errorf("stackqldoc: no resource %q", name)
	}
	sel := res.SQLVerbs["select"]
	if len(sel) == 0 {
		return nil, fmt.Errorf("stackqldoc: resource %q declares no select verb", name)
	}
	// sqlVerbs point at a method by $ref rather than naming it, so the last pointer segment is the
	// method key. Following the document's own indirection keeps this honest: whatever the doc says
	// backs SELECT is what runs.
	mName := lastSegment(sel[0].Ref)
	m, ok := res.Methods[mName]
	if !ok {
		return nil, fmt.Errorf("stackqldoc: resource %q select references unknown method %q", name, mName)
	}
	path, verb, err := splitOperationRef(m.Operation.Ref)
	if err != nil {
		return nil, fmt.Errorf("stackqldoc: resource %q method %q: %w", name, mName, err)
	}
	ops, ok := d.Paths[path]
	if !ok {
		return nil, fmt.Errorf("stackqldoc: resource %q method %q: no path %q", name, mName, path)
	}
	op, ok := ops[verb]
	if !ok {
		return nil, fmt.Errorf("stackqldoc: resource %q method %q: path %q has no %q", name, mName, path, verb)
	}
	if len(d.Servers) == 0 {
		return nil, fmt.Errorf("stackqldoc: document declares no servers")
	}
	return d.build(name, verb, path, op, m)
}

// build turns a resolved (server, path, operation, method) into an Exchange.
func (d *document) build(name, verb, path string, op pathOp, m method) (Exchange, error) {
	srv := d.Servers[0]
	base, vars := srv.URL, serverVars(srv)

	// The path key carries the operation's identity as __-prefixed pseudo-parameters
	// (/?__Action=DescribeInstances&__Version=2016-11-15). stackql's requestTranslate
	// drop_double_underscore_params says they are NOT query parameters: with a form request they are
	// the body that names the action. Stripping the marker prefix recovers the real call.
	route, action := splitPseudoParams(path)
	req := httpx.Request{
		Method: strings.ToUpper(verb),
		URL:    strings.TrimRight(base, "/") + route,
	}
	if len(action) > 0 && encodingOf(m.Request.MediaType) == httpx.EncodingForm {
		req.Body = httpx.Body{Encoding: httpx.EncodingForm, Params: action}
	}

	return &exchange{
		name:    name,
		in:      vars,
		req:     req,
		decode:  decoderFor(m.Response.MediaType),
		objKey:  m.Response.ObjectKey,
		pageIn:  m.Config.Pagination.RequestToken,
		pageOut: m.Config.Pagination.ResponseToken,
		opID:    op.OperationID,
	}, nil
}

// serverVars are the server URL's {variables}, which the exchange binds as inputs — a region is a
// scope decision the caller makes, never something a parser invents. Sorted for a stable signature.
func serverVars(s server) []string {
	out := make([]string, 0, len(s.Variables))
	for k := range s.Variables {
		if strings.Contains(s.URL, "{"+k+"}") {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// splitPseudoParams separates a path key's real route from its __-prefixed pseudo-parameters,
// returning the route and the parameters with the marker prefix removed.
func splitPseudoParams(path string) (route string, params map[string]any) {
	route, query, found := strings.Cut(path, "?")
	if !found {
		return path, nil
	}
	params = map[string]any{}
	var kept []string
	for _, kv := range strings.Split(query, "&") {
		k, v, _ := strings.Cut(kv, "=")
		if name, isPseudo := strings.CutPrefix(k, "__"); isPseudo {
			params[name] = v
			continue
		}
		kept = append(kept, kv)
	}
	if len(kept) > 0 {
		route += "?" + strings.Join(kept, "&")
	}
	return route, params
}

// splitOperationRef decodes a "#/paths/<escaped-path>/<verb>" JSON pointer. Path separators inside
// the path are escaped as ~1 (and ~ as ~0), so the LAST unescaped "/" is the verb boundary.
func splitOperationRef(r string) (path, verb string, err error) {
	const prefix = "#/paths/"
	if !strings.HasPrefix(r, prefix) {
		return "", "", fmt.Errorf("operation $ref %q is not a #/paths pointer", r)
	}
	rest := strings.TrimPrefix(r, prefix)
	i := strings.LastIndex(rest, "/")
	if i < 0 {
		return "", "", fmt.Errorf("operation $ref %q names no verb", r)
	}
	return unescapePointer(rest[:i]), rest[i+1:], nil
}

// unescapePointer reverses RFC 6901 token escaping (~1 → "/", ~0 → "~"), in that order.
func unescapePointer(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~1", "/"), "~0", "~")
}

func lastSegment(ref string) string {
	i := strings.LastIndex(ref, "/")
	if i < 0 {
		return ref
	}
	return ref[i+1:]
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

// decoderFor is the transform that turns a response body of this media type into an agnostic
// document. nil when the type is not one we decode, so the body passes through raw rather than
// being silently mangled.
func decoderFor(mediaType string) facade.Transform {
	switch {
	case strings.Contains(mediaType, "xml"):
		return transform.NewXMLToAgnostic()
	case strings.Contains(mediaType, "json"):
		return transform.NewJSONToAgnostic()
	default:
		return nil
	}
}

// ---- exchange ---------------------------------------------------------------

type exchange struct {
	name    string
	in      []string
	req     httpx.Request
	decode  facade.Transform
	objKey  string
	pageIn  tokenSpec
	pageOut tokenSpec
	opID    string
}

func (e *exchange) Name() string { return e.name }
func (e *exchange) In() []string { return e.in }

// Out is empty: this exchange publishes the decoded response document rather than named attributes.
// Naming attributes means projecting the provider's payload, which the document expresses as a
// response transform — see Request/ObjectKey on the parsed method.
func (e *exchange) Out() []string { return nil }

func (e *exchange) Make(bound map[string]any) facade.Operator {
	return httpx.MakeAgnostic(e.req)(bound)
}

// Flatten is nil: the generic merge applies.
func (e *exchange) Flatten() facade.Transform { return nil }

// OperationID is the document's own name for the backing operation, for tracing what was resolved.
func (e *exchange) OperationID() string { return e.opID }

// Request is the resolved request this exchange issues, exposed so a caller (or a test) can assert
// what the document resolved to without running it.
func (e *exchange) Request() httpx.Request { return e.req }

// ObjectKey is the document's declared path to the item list within the transformed response.
func (e *exchange) ObjectKey() string { return e.objKey }
