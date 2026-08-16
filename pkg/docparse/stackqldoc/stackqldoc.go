// Package stackqldoc parses a stackql provider document (OpenAPI plus the x-stackQL-resources
// extension) into AOT exchanges. It IMPLEMENTS the pkg/docparse/aot contract; it is one dialect, and
// a consumer depends on that contract rather than on this parser. Input is document BYTES; output is a collection of exchanges.
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

	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
)

// Doc is a parsed provider document.
type Doc interface {
	// Resources are the resource names the document declares, sorted.
	Resources() []string
	// Select returns the exchange backing a resource's SELECT verb. A resource with no SELECT is an
	// error — a real answer about the provider, not an empty result.
	Select(resource string) (aot.AOTExchange, error)
	// Methods are a resource's methods with the SQL verb each is mapped to, sorted.
	Methods(resource string) ([]aot.Method, error)
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
	Servers []server `yaml:"servers"`
	// A path item holds verbs alongside non-operation keys (an OpenAPI path-level `parameters`
	// list, vendor extensions), so verbs are decoded on demand rather than assumed.
	Paths      map[string]map[string]yaml.Node `yaml:"paths"`
	Security   []map[string][]string           `yaml:"security"`
	Components struct {
		Resources       map[string]resource       `yaml:"x-stackQL-resources"`
		SecuritySchemes map[string]securityScheme `yaml:"securitySchemes"`
	} `yaml:"components"`
}

// securityScheme is an OpenAPI scheme. AWS documents describe SigV4 as an apiKey scheme carrying the
// Authorization header, and say what it REALLY is only in the vendor extension — so that extension is
// the thing to read, not the type.
type securityScheme struct {
	Type     string `yaml:"type"`
	AuthType string `yaml:"x-amazon-apigateway-authtype"`
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

func (d *document) Select(name string) (aot.AOTExchange, error) {
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
	node, ok := ops[verb]
	if !ok {
		return nil, fmt.Errorf("stackqldoc: resource %q method %q: path %q has no %q", name, mName, path, verb)
	}
	var op pathOp
	if err := node.Decode(&op); err != nil {
		return nil, fmt.Errorf("stackqldoc: resource %q method %q: decode %s %s: %w", name, mName, verb, path, err)
	}
	if len(d.Servers) == 0 {
		return nil, fmt.Errorf("stackqldoc: document declares no servers")
	}
	return d.build(name, verb, path, op, m), nil
}

// Methods lists a resource's methods and the verb each is bound to. The verb comes from sqlVerbs,
// which points at methods by $ref — so the mapping is read the same way SELECT is resolved, and a
// method the document maps to nothing is reported as exactly that rather than omitted.
func (d *document) Methods(name string) ([]aot.Method, error) {
	res, ok := d.Components.Resources[name]
	if !ok {
		return nil, fmt.Errorf("stackqldoc: no resource %q", name)
	}
	verbOf := map[string]string{}
	for verb, refs := range res.SQLVerbs {
		for _, r := range refs {
			verbOf[lastSegment(r.Ref)] = verb
		}
	}
	out := make([]aot.Method, 0, len(res.Methods))
	for mName, m := range res.Methods {
		out = append(out, methodInfo{name: mName, verb: verbOf[mName], opID: d.operationID(m)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out, nil
}

// operationID resolves a method's backing operation id, empty when it cannot be resolved — a listing
// must not fail because one method's pointer is broken.
func (d *document) operationID(m method) string {
	path, verb, err := splitOperationRef(m.Operation.Ref)
	if err != nil {
		return ""
	}
	node, ok := d.Paths[path][verb]
	if !ok {
		return ""
	}
	var op pathOp
	if err := node.Decode(&op); err != nil {
		return ""
	}
	return op.OperationID
}

type methodInfo struct{ name, verb, opID string }

func (m methodInfo) Name() string        { return m.name }
func (m methodInfo) SQLVerb() string     { return m.verb }
func (m methodInfo) OperationID() string { return m.opID }

// build turns a resolved (server, path, operation, method) into an AOT exchange.
func (d *document) build(name, verb, path string, op pathOp, m method) aot.AOTExchange {
	srv := d.Servers[0]

	// The path key carries the operation's identity as __-prefixed pseudo-parameters
	// (/?__Action=DescribeInstances&__Version=2016-11-15). stackql's requestTranslate
	// drop_double_underscore_params says they are NOT query parameters: with a form request they are
	// the body that names the action. Stripping the marker prefix recovers the real call.
	route, params := splitPseudoParams(path)

	return &aotExchange{
		name:   name,
		sec:    d.security(),
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

// security resolves the document-level requirement to a normalized scheme. Document-level is the only
// level these documents use; an operation-level override would resolve here too.
func (d *document) security() security {
	for _, req := range d.Security {
		for schemeName := range req {
			s, ok := d.Components.SecuritySchemes[schemeName]
			if !ok {
				continue
			}
			if strings.EqualFold(s.AuthType, "awsSigv4") {
				return security{scheme: aot.SchemeAWSSigV4, name: schemeName}
			}
		}
	}
	return security{}
}

type security struct {
	scheme aot.Scheme
	name   string
}

func (s security) Scheme() aot.Scheme { return s.scheme }
func (s security) Name() string       { return s.name }

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
	sec    security
	inputs []string
	opID   string
	req    request
	resp   response
}

func (e *aotExchange) Name() string           { return e.name }
func (e *aotExchange) Inputs() []string       { return e.inputs }
func (e *aotExchange) OperationID() string    { return e.opID }
func (e *aotExchange) Request() aot.Request   { return e.req }
func (e *aotExchange) Response() aot.Response { return e.resp }
func (e *aotExchange) Security() aot.Security { return e.sec }

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

func (r response) MediaType() string          { return r.mediaType }
func (r response) OverrideMediaType() string  { return r.override }
func (r response) ObjectKey() string          { return r.objectKey }
func (r response) Transform() aot.Transform   { return r.transform }
func (r response) Pagination() aot.Pagination { return r.pagination }

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
