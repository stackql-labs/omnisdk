// Package stackqldoc parses a stackql provider document (OpenAPI plus the x-stackQL-resources
// extension) into AOT exchanges. Input is document BYTES; output is a collection of exchanges.
//
// An AOT exchange is a DESCRIPTION, not a binding: what the document says the call is. It is
// deliberately not the engine's plan-time exchange, which is an executable thing (Make → Operator,
// Flatten) whose transforms are evaluated lazily at the sink. The document attaches its response
// transform to the SOURCE; the plan applies transforms at the SINK. Relocating one to the other is a
// real decision, so it belongs in an explicit compile step against these interfaces — not smuggled in
// by hanging both concerns off one type. Nothing here imports the engine.
package stackqldoc

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

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

// Doc is a parsed provider document.
type Doc interface {
	// Resources are the resource names the document declares, sorted.
	Resources() []string
	// Select returns the exchange backing a resource's SELECT verb. A resource with no SELECT is an
	// error — a real answer about the provider, not an empty result.
	Select(resource string) (AOTExchange, error)
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
		MediaType         string        `yaml:"mediaType"`
		OverrideMediaType string        `yaml:"overrideMediaType"`
		ObjectKey         string        `yaml:"objectKey"`
		Transform         transformSpec `yaml:"transform"`
	} `yaml:"response"`
}

type transformSpec struct {
	Type string `yaml:"type"`
	Body string `yaml:"body"`
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

func (d *document) Select(name string) (AOTExchange, error) {
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
	return d.build(name, verb, path, op, m), nil
}

// build turns a resolved (server, path, operation, method) into an AOT exchange.
func (d *document) build(name, verb, path string, op pathOp, m method) AOTExchange {
	srv := d.Servers[0]

	// The path key carries the operation's identity as __-prefixed pseudo-parameters
	// (/?__Action=DescribeInstances&__Version=2016-11-15). stackql's requestTranslate
	// drop_double_underscore_params says they are NOT query parameters: with a form request they are
	// the body that names the action. Stripping the marker prefix recovers the real call.
	route, params := splitPseudoParams(path)

	return &aotExchange{
		name:   name,
		inputs: serverVars(srv),
		opID:   op.OperationID,
		req: request{
			method:    strings.ToUpper(verb),
			url:       strings.TrimRight(srv.URL, "/") + route,
			mediaType: m.Request.MediaType,
			params:    params,
		},
		resp: response{
			mediaType:  m.Response.MediaType,
			override:   m.Response.OverrideMediaType,
			objectKey:  m.Response.ObjectKey,
			transform:  transformDecl(m.Response.Transform),
			pagination: pagination{req: m.Config.Pagination.RequestToken, resp: m.Config.Pagination.ResponseToken},
		},
	}
}

// serverVars are the server URL's {variables}, which become bound inputs — a region is a scope
// decision the caller makes, never something a parser invents. Sorted for a stable signature.
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
func splitPseudoParams(path string) (route string, params map[string]string) {
	route, query, found := strings.Cut(path, "?")
	if !found {
		return path, nil
	}
	params = map[string]string{}
	var kept []string
	for _, kv := range strings.Split(query, "&") {
		k, v, _ := strings.Cut(kv, "=")
		if pname, isPseudo := strings.CutPrefix(k, "__"); isPseudo {
			params[pname] = v
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

// ---- AOT exchange -----------------------------------------------------------

type aotExchange struct {
	name   string
	inputs []string
	opID   string
	req    request
	resp   response
}

func (e *aotExchange) Name() string        { return e.name }
func (e *aotExchange) Inputs() []string    { return e.inputs }
func (e *aotExchange) OperationID() string { return e.opID }
func (e *aotExchange) Request() Request    { return e.req }
func (e *aotExchange) Response() Response  { return e.resp }

type request struct {
	method    string
	url       string
	mediaType string
	params    map[string]string
}

func (r request) Method() string    { return r.method }
func (r request) URL() string       { return r.url }
func (r request) MediaType() string { return r.mediaType }

func (r request) Params() map[string]string {
	out := make(map[string]string, len(r.params))
	for k, v := range r.params {
		out[k] = v
	}
	return out
}

type response struct {
	mediaType  string
	override   string
	objectKey  string
	transform  transformDeclType
	pagination pagination
}

func (r response) MediaType() string         { return r.mediaType }
func (r response) OverrideMediaType() string { return r.override }
func (r response) ObjectKey() string         { return r.objectKey }
func (r response) Transform() Transform      { return r.transform }
func (r response) Pagination() Pagination    { return r.pagination }

type transformDeclType struct{ typ, body string }

func transformDecl(t transformSpec) transformDeclType {
	return transformDeclType{typ: t.Type, body: t.Body}
}

func (t transformDeclType) Type() string { return t.typ }
func (t transformDeclType) Body() string { return t.body }

type pagination struct{ req, resp tokenSpec }

func (p pagination) RequestToken() (string, string)  { return p.req.Key, p.req.Location }
func (p pagination) ResponseToken() (string, string) { return p.resp.Key, p.resp.Location }
func (p pagination) Declared() bool                  { return p.req.Key != "" || p.resp.Key != "" }
