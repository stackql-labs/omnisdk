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
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
	"github.com/stackql-labs/omnisdk/pkg/namespace"
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
	// Selects are every exchange the resource's SELECT verb names, in declaration order.
	Selects(resource string) ([]aot.AOTExchange, error)
	// Verb are every exchange the resource binds to one SQL verb, in declaration order. Selects is
	// this with "select"; a mutating verb is reached the same way, since nothing about resolving an
	// operation depends on which verb named it.
	Verb(resource, verb string) ([]aot.AOTExchange, error)
}

// Parse reads a stackql provider document.
func Parse(b []byte) (Doc, error) {
	var d document
	sum := sha256.Sum256(b)
	d.fingerprint = hex.EncodeToString(sum[:])
	if err := yaml.Unmarshal(b, &d); err != nil {
		return nil, fmt.Errorf("stackqldoc: parse: %w", err)
	}
	if len(d.Components.Resources) == 0 {
		d.Components.Resources = d.ResourcesRoot
	}
	if len(d.Components.Resources) == 0 {
		return nil, fmt.Errorf("stackqldoc: no x-stackQL-resources in document")
	}
	return &d, nil
}

// ---- document shape (only the parts an exchange needs) ----------------------

type document struct {
	fingerprint string
	Info        struct {
		ServiceName string `yaml:"x-serviceName"`
	} `yaml:"info"`
	source        string
	resolver      documentResolver
	ResourcesRoot map[string]resource `yaml:"resources"`
	ServiceDoc    ref                 `yaml:"serviceDoc"`
	// providerName is the provider the document belongs to, set by the catalog that read it. It
	// matters only where the document says nothing: see pagination.
	providerName string
	snakeAliases bool
	Servers      []server `yaml:"servers"`
	// Config is the document-wide stackql configuration; its pagination applies to every method
	// that states none of its own.
	Config struct {
		Retry               *retrySpec     `yaml:"retry"`
		RequestTranslate    algorithmSpec  `yaml:"requestTranslate"`
		QueryParamTranspose algorithmSpec  `yaml:"queryParamTranspose"`
		Pagination          paginationSpec `yaml:"pagination"`
		QueryParamPushdown  *pushdownSpec  `yaml:"queryParamPushdown"`
	} `yaml:"x-stackQL-config"`
	// A path item holds verbs alongside non-operation keys (an OpenAPI path-level `parameters`
	// list, vendor extensions), so verbs are decoded on demand rather than assumed.
	Paths      map[string]map[string]yaml.Node `yaml:"paths"`
	Security   []map[string][]string           `yaml:"security"`
	Components struct {
		Resources       map[string]resource       `yaml:"x-stackQL-resources"`
		SecuritySchemes map[string]securityScheme `yaml:"securitySchemes"`
		// Schemas are retained as raw nodes. A service document's schema section is the bulk of its
		// bytes — EC2's runs to tens of thousands of lines — and an exchange needs the handful of
		// shapes its own response names, so they are read on demand rather than decoded up front.
		Schemas map[string]yaml.Node `yaml:"schemas"`
		// Parameters are shared input declarations an operation reaches by $ref.
		Parameters map[string]pathParam `yaml:"parameters"`
		// Responses are shared response declarations an operation reaches by $ref.
		Responses map[string]opResponse `yaml:"responses"`
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
	Default     string `yaml:"default"`
	Environment string `yaml:"x-stackQL-envVar"`
}

type resource struct {
	Name     string            `yaml:"name"`
	ID       string            `yaml:"id"`
	Title    string            `yaml:"title"`
	Methods  map[string]method `yaml:"methods"`
	SQLVerbs map[string][]ref  `yaml:"sqlVerbs"`
	Config   struct {
		Retry               *retrySpec           `yaml:"retry"`
		Views               map[string]*viewSpec `yaml:"views"`
		RequestTranslate    algorithmSpec        `yaml:"requestTranslate"`
		QueryParamTranspose algorithmSpec        `yaml:"queryParamTranspose"`
		Pagination          paginationSpec       `yaml:"pagination"`
		QueryParamPushdown  *pushdownSpec        `yaml:"queryParamPushdown"`
	} `yaml:"config"`
}

type ref struct {
	Ref string `yaml:"$ref"`
}

type algorithmSpec struct {
	Algorithm string `yaml:"algorithm"`
}

type method struct {
	Inverse struct {
		Method ref `yaml:"sqlVerb"`
		Tokens map[string]struct {
			Key       string `yaml:"key"`
			Location  string `yaml:"location"`
			Algorithm string `yaml:"algorithm"`
		} `yaml:"tokens"`
	} `yaml:"inverse"`
	Inline      []string             `yaml:"inline"`
	Parameters  map[string]pathParam `yaml:"parameters"`
	ServiceName string               `yaml:"serviceName"`
	Operation   ref                  `yaml:"operation"`
	Path        ref                  `yaml:"path"`
	Config      struct {
		RequestBodyTranslate algorithmSpec  `yaml:"requestBodyTranslate"`
		Retry                *retrySpec     `yaml:"retry"`
		RequestTranslate     algorithmSpec  `yaml:"requestTranslate"`
		QueryParamTranspose  algorithmSpec  `yaml:"queryParamTranspose"`
		QueryParamPushdown   *pushdownSpec  `yaml:"queryParamPushdown"`
		Pagination           paginationSpec `yaml:"pagination"`
	} `yaml:"config"`
	Request struct {
		Required          []string      `yaml:"required"`
		Transform         transformSpec `yaml:"transform"`
		XMLDeclaration    string        `yaml:"xmlDeclaration"`
		XMLRootAnnotation string        `yaml:"xmlRootAnnotation"`
		Default           string        `yaml:"default"`
		Base              string        `yaml:"base"`
		MediaType         string        `yaml:"mediaType"`
		NativeCasing      string        `yaml:"nativeCasing"`
	} `yaml:"request"`
	Response struct {
		SchemaOverride    yaml.Node     `yaml:"schema_override"`
		MediaType         string        `yaml:"mediaType"`
		OverrideMediaType string        `yaml:"overrideMediaType"`
		ObjectKey         string        `yaml:"objectKey"`
		Transform         transformSpec `yaml:"transform"`
		// OpenAPIDocKey names the response code whose schema the rows follow.
		OpenAPIDocKey string `yaml:"openAPIDocKey"`
	} `yaml:"response"`
	// Beta is the call that completes this one, where it answers before what it made exists.
	Beta *betaSpec `yaml:"x-stackQL-beta"`
}

// betaSpec is a method's x-stackQL-beta: the method that follows it, what binds that method's
// inputs, and when it reports done.
type betaSpec struct {
	Method ref               `yaml:"method"`
	Bind   map[string]string `yaml:"bind"`
	Await  struct {
		StatusPath  string `yaml:"statusPath"`
		Done        string `yaml:"done"`
		ErrorPath   string `yaml:"errorPath"`
		Interval    string `yaml:"interval"`
		MaxAttempts int    `yaml:"maxAttempts"`
	} `yaml:"await"`
}

type transformSpec struct {
	Type string `yaml:"type"`
	Body string `yaml:"body"`
}

type tokenSpec struct {
	Key      string `yaml:"key"`
	Location string `yaml:"location"`
	Encoding string `yaml:"encoding"`
}

type paginationSpec struct {
	Algorithm          string    `yaml:"algorithm"`
	ResponseTerminator tokenSpec `yaml:"responseTerminator"`
	RequestToken       tokenSpec `yaml:"requestToken"`
	ResponseToken      tokenSpec `yaml:"responseToken"`
}

type pathOp struct {
	MsPageable *struct {
		NextLinkName *string `yaml:"nextLinkName"`
	} `yaml:"x-ms-pageable"`
	Servers     []server     `yaml:"servers"`
	GraphQL     *graphQLSpec `yaml:"x-stackQL-graphQL"`
	OperationID string       `yaml:"operationId"`
	Parameters  []pathParam  `yaml:"parameters"`
	// RequestBody declares the media type and schema used to validate caller-supplied fields.
	RequestBody struct {
		Required bool `yaml:"required"`
		Content  map[string]struct {
			Schema yaml.Node `yaml:"schema"`
		} `yaml:"content"`
	} `yaml:"requestBody"`
	// Responses carry the declared response shape. Only the success response is read: an error
	// response describes a failure, and projecting rows out of one would be reporting a fault as
	// data.
	Responses map[string]opResponse `yaml:"responses"`
}

// opResponse is a declared response: its content, or a $ref to a shared one under
// components/responses (Microsoft Graph states every collection response that way).
type opResponse struct {
	Ref     string `yaml:"$ref"`
	Content map[string]struct {
		Schema yaml.Node `yaml:"schema"`
	} `yaml:"content"`
}

// pathParam is an operation parameter as the document declares it.
type pathParam struct {
	Name     string `yaml:"name"`
	In       string `yaml:"in"`
	Required bool   `yaml:"required"`
	Schema   struct {
		Default any    `yaml:"default"`
		Type    string `yaml:"type"`
		Ref     string `yaml:"$ref"`
	} `yaml:"schema"`
	// Ref points at a shared declaration under components/parameters, in place of the fields.
	Ref string `yaml:"$ref"`
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
	exs, err := d.Selects(name)
	if err != nil {
		return nil, err
	}
	return exs[0], nil
}

// Selects resolves EVERY method the resource binds to SELECT. Documents routinely bind more than one
// — a get by identifier and a list by scope — and picking the first would silently answer a different
// question from the one asked.
func (d *document) Selects(name string) ([]aot.AOTExchange, error) {
	return d.Verb(name, "select")
}

// Verb resolves every method a resource binds to one SQL verb, in the order the document lists them.
// That order is the selection rule: a verb fans out — a resource declares both CreateSubnet and
// CreateDefaultSubnet under insert — and the caller's inputs decide which applies, so the choice is
// made by matching signatures down this list rather than here.
func (d *document) Verb(name, verb string) ([]aot.AOTExchange, error) {
	res, ok := d.Components.Resources[name]
	if !ok {
		return nil, fmt.Errorf("stackqldoc: no resource %q", name)
	}
	sel, explicit := res.SQLVerbs[verb]
	if verb == "exec" {
		names := make([]string, 0, len(res.Methods))
		for method := range res.Methods {
			names = append(names, method)
		}
		slices.Sort(names)
		for _, method := range names {
			sel = append(sel, ref{Ref: "#/methods/" + method})
		}
		explicit = true
	}
	if !explicit {
		for _, key := range defaultMethodKeys(verb) {
			if _, exists := res.Methods[key]; exists {
				sel = append(sel, ref{Ref: "#/methods/" + key})
			}
		}
	}
	if len(sel) == 0 {
		return nil, fmt.Errorf("stackqldoc: resource %q declares no %s verb", name, verb)
	}
	var out []aot.AOTExchange
	var firstErr error
	for _, ref := range sel {
		// sqlVerbs point at a method by $ref rather than naming it, so the last pointer segment is the
		// method key. Following the document's own indirection keeps this honest.
		ex, err := d.resolve(name, lastSegment(ref.Ref), true)
		if err != nil {
			firstErr = orErr(firstErr, err)
			continue
		}
		out = append(out, ex)
	}
	if len(out) == 0 {
		if firstErr != nil {
			return nil, firstErr
		}
		return nil, fmt.Errorf("stackqldoc: resource %q %s resolves to nothing", name, verb)
	}
	return out, nil
}

// resolve builds one method of a resource. withBeta follows the method's x-stackQL-beta; the method a
// beta names is resolved without its own, so a declaration cannot chain without end.
func (d *document) resolve(name, mName string, withBeta bool) (aot.AOTExchange, error) {
	res, ok := d.Components.Resources[name]
	if !ok {
		return nil, fmt.Errorf("stackqldoc: no resource %q", name)
	}
	m, ok := res.Methods[mName]
	if !ok {
		return nil, fmt.Errorf("stackqldoc: resource %q references unknown method %q", name, mName)
	}
	if len(m.Inline) > 0 {
		op := pathOp{Servers: []server{{URL: "local://command"}}}
		for name, param := range m.Parameters {
			param.Name = name
			op.Parameters = append(op.Parameters, param)
		}
		slices.SortFunc(op.Parameters, func(a, b pathParam) int { return strings.Compare(a.Name, b.Name) })
		ex := d.build(mName, "POST", "", op, m).(*aotExchange)
		ex.inline = slices.Clone(m.Inline)
		return ex, nil
	}
	// Older documents state the path and verb separately.
	if m.Path.Ref != "" && !strings.Contains(m.Operation.Ref, "#") {
		escaped := strings.ReplaceAll(strings.ReplaceAll(m.Path.Ref, "~", "~0"), "/", "~1")
		m.Operation.Ref = "#/paths/" + escaped + "/" + strings.ToLower(m.Operation.Ref)
	}
	target, resolved, err := d.operationDocument(name, mName, m)
	if err != nil {
		return nil, err
	}
	if target != d {
		return target.resolve(name, mName, withBeta)
	}
	m = resolved
	path, httpVerb, err := splitOperationRef(m.Operation.Ref)
	if err != nil {
		return nil, fmt.Errorf("stackqldoc: resource %q method %q: %w", name, mName, err)
	}
	node, ok := d.Paths[path][httpVerb]
	if !ok {
		return nil, fmt.Errorf("stackqldoc: resource %q method %q: path %q has no %q", name, mName, path, httpVerb)
	}
	var op pathOp
	if err := node.Decode(&op); err != nil {
		return nil, fmt.Errorf("stackqldoc: resource %q method %q: decode %s %s: %w", name, mName, httpVerb, path, err)
	}
	if len(op.Servers) == 0 {
		if declaration, present := d.Paths[path]["servers"]; present {
			if err := declaration.Decode(&op.Servers); err != nil {
				return nil, fmt.Errorf("stackqldoc: path %q servers: %w", path, err)
			}
		}
	}
	if len(op.Servers) == 0 {
		op.Servers = d.Servers
	}
	if len(op.Servers) == 0 {
		return nil, fmt.Errorf("stackqldoc: document declares no servers")
	}
	params, err := d.parameters(path, op.Parameters)
	if err != nil {
		return nil, fmt.Errorf("stackqldoc: resource %q method %q: %w", name, mName, err)
	}
	op.Parameters = params
	if m.Config.RequestTranslate.Algorithm == "" {
		m.Config.RequestTranslate = res.Config.RequestTranslate
		if m.Config.RequestTranslate.Algorithm == "" {
			m.Config.RequestTranslate = d.Config.RequestTranslate
		}
	}
	if m.Config.QueryParamTranspose.Algorithm == "" {
		m.Config.QueryParamTranspose = res.Config.QueryParamTranspose
		if m.Config.QueryParamTranspose.Algorithm == "" {
			m.Config.QueryParamTranspose = d.Config.QueryParamTranspose
		}
	}
	if p := m.Config.Pagination; p.Algorithm == "" && p.RequestToken.Key == "" && p.ResponseToken.Key == "" {
		m.Config.Pagination = res.Config.Pagination
	}
	ex := d.build(mName, httpVerb, path, op, m)
	for _, retry := range []*retrySpec{m.Config.Retry, res.Config.Retry, d.Config.Retry} {
		if retry != nil {
			ex.(*aotExchange).retry = retry
			break
		}
	}
	// The nearest declaration wins: the method's, else its resource's, else the document's.
	for _, spec := range []*pushdownSpec{m.Config.QueryParamPushdown, res.Config.QueryParamPushdown, d.Config.QueryParamPushdown} {
		if spec != nil {
			ex.(*aotExchange).pushdown = spec
			break
		}
	}
	if !withBeta || m.Beta == nil {
		return ex, nil
	}
	b, err := d.beta(*m.Beta)
	if err != nil {
		// A beta that cannot be followed is an error, never dropped: without it the operation would
		// read as finished the moment it answers.
		return nil, fmt.Errorf("stackqldoc: resource %q method %q: x-stackQL-beta: %w", name, mName, err)
	}
	ex.(*aotExchange).beta = b
	return ex, nil
}

// beta resolves a declared x-stackQL-beta. Its method is a pointer into this document's resources,
// #/components/x-stackQL-resources/<resource>/methods/<method>, and every await field but the error
// path is required.
func (d *document) beta(spec betaSpec) (aot.Beta, error) {
	const prefix = "#/components/x-stackQL-resources/"
	resName, mName, ok := strings.Cut(strings.TrimPrefix(spec.Method.Ref, prefix), "/methods/")
	if !strings.HasPrefix(spec.Method.Ref, prefix) || !ok || resName == "" || mName == "" {
		return nil, fmt.Errorf("method $ref %q is not a %s<resource>/methods/<method> pointer", spec.Method.Ref, prefix)
	}
	follow, err := d.resolve(unescapePointer(resName), unescapePointer(mName), false)
	if err != nil {
		return nil, err
	}
	a := spec.Await
	interval, err := time.ParseDuration(a.Interval)
	switch {
	case a.StatusPath == "" || a.Done == "":
		return nil, fmt.Errorf("await needs statusPath and done")
	case err != nil || interval <= 0:
		return nil, fmt.Errorf("await interval %q is not a positive duration", a.Interval)
	case a.MaxAttempts <= 0:
		return nil, fmt.Errorf("await needs a positive maxAttempts")
	}
	return beta{method: follow, bind: spec.Bind, await: await{status: a.StatusPath, done: a.Done,
		errPath: a.ErrorPath, interval: interval, attempts: a.MaxAttempts}}, nil
}

type beta struct {
	method aot.AOTExchange
	bind   map[string]string
	await  await
}

func (b beta) Method() aot.AOTExchange { return b.method }
func (b beta) Bind() map[string]string { return b.bind }
func (b beta) Await() aot.Await        { return b.await }

type await struct {
	status, done, errPath string
	interval              time.Duration
	attempts              int
}

func (a await) StatusPath() string      { return a.status }
func (a await) Done() string            { return a.done }
func (a await) ErrorPath() string       { return a.errPath }
func (a await) Interval() time.Duration { return a.interval }
func (a await) MaxAttempts() int        { return a.attempts }

// parameters are an operation's inputs with every $ref followed and the path's own parameters
// included, as OpenAPI defines them: a path-level parameter applies to each of its operations unless
// the operation declares one with the same name and location. A shared declaration is where most
// generated documents put a common input (GitHub's org, username), so dropping a $ref drops the very
// parameter a join binds. A $ref that resolves to nothing is an error, not an omission.
func (d *document) parameters(path string, own []pathParam) ([]pathParam, error) {
	var shared []pathParam
	if node, ok := d.Paths[path]["parameters"]; ok {
		if err := node.Decode(&shared); err != nil {
			return nil, fmt.Errorf("decode parameters of %s: %w", path, err)
		}
	}
	resolve := func(ps []pathParam) ([]pathParam, error) {
		out := make([]pathParam, 0, len(ps))
		for _, p := range ps {
			for hops := 0; p.Ref != ""; hops++ {
				target, ok := d.Components.Parameters[strings.TrimPrefix(p.Ref, "#/components/parameters/")]
				if !ok || hops > 8 {
					return nil, fmt.Errorf("parameter %s resolves to nothing", p.Ref)
				}
				p = target
			}
			out = append(out, p)
		}
		return out, nil
	}
	pathLevel, err := resolve(shared)
	if err != nil {
		return nil, err
	}
	opLevel, err := resolve(own)
	if err != nil {
		return nil, err
	}
	declared := map[string]bool{}
	for _, p := range opLevel {
		declared[p.In+"\x00"+p.Name] = true
	}
	for _, p := range pathLevel {
		if !declared[p.In+"\x00"+p.Name] {
			opLevel = append(opLevel, p)
		}
	}
	return opLevel, nil
}

func orErr(first, next error) error {
	if first != nil {
		return first
	}
	return next
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
	verbs := make([]string, 0, len(res.SQLVerbs))
	for verb := range res.SQLVerbs {
		verbs = append(verbs, verb)
	}
	sort.Strings(verbs)
	verbs = append([]string{"select", "insert", "delete", "update", "replace", "exec"}, verbs...)
	for _, verb := range verbs {
		for _, r := range res.SQLVerbs[verb] {
			name := lastSegment(r.Ref)
			if verbOf[name] == "" {
				verbOf[name] = verb
			}
		}
	}
	out := make([]aot.Method, 0, len(res.Methods))
	for mName, m := range res.Methods {
		verb := verbOf[mName]
		if verb == "" {
			for _, candidate := range []string{"select", "insert", "delete"} {
				if slices.Contains(defaultMethodKeys(candidate), mName) {
					verb = candidate
					break
				}
			}
		}
		info := methodInfo{name: mName, verb: verb, opID: d.operationID(m)}
		if ex, err := d.resolve(name, mName, false); err == nil {
			info.opID = ex.OperationID()
			if declared, ok := ex.Request().(aot.ServerTransportDefaults); ok {
				info.transport = declared.ServerTransportDefaults()
			}
			if declared, ok := ex.Request().(aot.ServerEnvironmentBindings); ok {
				info.environment = declared.ServerEnvironmentBindings()
			}
			naming := namespace.NewNaming(m.Request.NativeCasing, d.snakeAliases && m.Request.NativeCasing != "")
			var variables []string
			if declared, ok := ex.Request().(aot.ServerBindings); ok {
				variables = declared.ServerVariables()
			}
			var bodyRequired []string
			for _, p := range ex.Request().Parameters() {
				if p.Required() && !slices.Contains(variables, p.Name()) {
					name := naming.Surface(p.Name())
					if p.In() == aot.InBody {
						bodyRequired = append(bodyRequired, "data__"+name)
						continue
					}
					if !slices.Contains(info.required, name) {
						info.required = append(info.required, name)
					}
				}
			}
			sort.Strings(info.required)
			info.required = append(info.required, bodyRequired...)
			for _, input := range append(variables, ex.Inputs()...) {
				name := naming.Surface(input)
				if !slices.Contains(info.required, name) {
					info.required = append(info.required, name)
				}
			}
		}
		out = append(out, info)
	}
	if view := res.Config.Views["select"]; view != nil {
		out = append(out, methodInfo{name: "__view__", verb: "select", required: view.requiredParams()})
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

type methodInfo struct {
	name, verb, opID string
	required         []string
	environment      map[string]string
	transport        map[string]string
}

func (m methodInfo) RequiredParams() []string { return append([]string(nil), m.required...) }

func (m methodInfo) ServerTransportDefaults() map[string]string { return maps.Clone(m.transport) }

func (m methodInfo) ServerEnvironmentBindings() map[string]string { return maps.Clone(m.environment) }

func (m methodInfo) Name() string        { return m.name }
func (m methodInfo) SQLVerb() string     { return m.verb }
func (m methodInfo) OperationID() string { return m.opID }

// build turns a resolved (server, path, operation, method) into an AOT exchange.
func (d *document) build(name, verb, path string, op pathOp, m method) aot.AOTExchange {
	srv := op.Servers[0]

	// The path key carries the operation's identity as __-prefixed pseudo-parameters
	// (/?__Action=DescribeInstances&__Version=2016-11-15). stackql's requestTranslate
	// drop_double_underscore_params says they are NOT query parameters: with a form request they are
	// the body that names the action. Stripping the marker prefix recovers the real call.
	route, params := splitPseudoParams(path)
	if m.Config.RequestTranslate.Algorithm == "drop_double_underscore_params" {
		params = nil
	}
	schema := d.responseSchemaAt(op, m.Response.OpenAPIDocKey, "", m.Response.MediaType, &m.Response.SchemaOverride)
	var graph aot.GraphQL
	var rowSchema aot.Schema
	if m.Response.ObjectKey != "" {
		rowSchema = d.responseSchemaAt(op, m.Response.OpenAPIDocKey, m.Response.ObjectKey, m.Response.MediaType, &m.Response.SchemaOverride)
	}
	if op.GraphQL != nil {
		graph = graphQL{op.GraphQL}
		selection := graph.Selection()
		if selection == "" {
			selection = m.Response.ObjectKey
		}
		rowSchema = d.responseSchemaAt(op, m.Response.OpenAPIDocKey, selection, m.Response.MediaType, &m.Response.SchemaOverride)
	}
	parameters := d.operationParams(op)
	// A mock or private-endpoint override can replace the host while retaining
	// declared server variables. They remain accepted scope bindings even when
	// the rewritten URL no longer needs them.
	environment := map[string]string{}
	variableNames := make([]string, 0, len(srv.Variables))
	for name, variable := range srv.Variables {
		if variable.Environment != "" {
			environment[name] = variable.Environment
		}
		variableNames = append(variableNames, name)
	}
	sort.Strings(variableNames)
	for _, name := range variableNames {
		if !slices.ContainsFunc(parameters, func(p aot.Parameter) bool { return p.Name() == name }) {
			parameters = append(parameters, parameter{p: pathParam{Name: name, In: aot.InPath}})
		}
	}
	url := strings.TrimRight(serverURL(srv.URL), "/") + route
	var bodySchema aot.Schema
	var bodyRootName string
	var bodyTypes []string
	for media := range op.RequestBody.Content {
		bodyTypes = append(bodyTypes, media)
	}
	sort.Strings(bodyTypes)
	for _, media := range bodyTypes {
		node := op.RequestBody.Content[media].Schema
		components := map[string]*yaml.Node{}
		for name, value := range d.Components.Schemas {
			v := value
			components[name] = &v
		}
		bodySchema = newSchema(&node, components)
		if declared, ok := bodySchema.(*materialised); ok && len(m.Request.Required) > 0 {
			copy := *declared
			copy.required = slices.Clone(declared.required)
			for _, name := range m.Request.Required {
				if !slices.Contains(copy.required, name) {
					copy.required = append(copy.required, name)
				}
			}
			bodySchema = &copy
		}
		bodyRootName = schemaXMLName(&node, components, 0)
		if bodyRootName == "" {
			if title := schemaField(&node, components, "title", 0); title != nil {
				bodyRootName = title.Value
			}
		}
		break
	}
	if bodySchema != nil {
		base := map[string]any{}
		_ = json.Unmarshal([]byte(m.Request.Base), &base)
		required := slices.Clone(m.Request.Required)
		if shape, ok := bodySchema.(aot.RequiredSchema); ok && op.RequestBody.Required {
			required = append(required, shape.RequiredProperties()...)
		}
		for _, name := range bodySchema.Properties() {
			if !slices.ContainsFunc(parameters, func(p aot.Parameter) bool { return p.Name() == name && p.In() == aot.InBody }) {
				property, _ := bodySchema.Property(name)
				_, list := property.Items()
				parameters = append(parameters, parameter{p: pathParam{Name: name, In: aot.InBody, Required: slices.Contains(required, name) && base[name] == nil}, list: list})
			}
		}
	}

	inverseTokens := map[string][3]string{}
	for name, t := range m.Inverse.Tokens {
		inverseTokens[name] = [3]string{t.Key, t.Location, t.Algorithm}
	}
	return &aotExchange{
		fingerprint: d.fingerprint, inverseRef: m.Inverse.Method.Ref, inverseTokens: inverseTokens,
		graphQL: graph,
		name:    name,
		sec:     d.security(),
		inputs:  serverVars(srv),
		opID:    op.OperationID,
		req: request{
			requestTransform:  transformDecl(m.Request.Transform),
			xmlRootName:       bodyRootName,
			xmlDeclaration:    m.Request.XMLDeclaration,
			xmlAnnotation:     m.Request.XMLRootAnnotation,
			bodyTranslation:   m.Config.RequestBodyTranslate.Algorithm,
			signingService:    cmp.Or(m.ServiceName, d.Info.ServiceName),
			defaultBody:       m.Request.Default,
			method:            strings.ToUpper(verb),
			url:               url,
			mediaType:         m.Request.MediaType,
			params:            params,
			parameters:        parameters,
			baseBody:          m.Request.Base,
			nativeCasing:      m.Request.NativeCasing,
			snakeAliases:      d.snakeAliases,
			serverVariables:   variableNames,
			serverEnvironment: environment,
			transportDefaults: serverTransportDefaults(srv),
			bodySchema:        bodySchema,
			bodyMediaType:     bodyMediaType(op),
			translate:         m.Config.RequestTranslate.Algorithm,
			transpose:         m.Config.QueryParamTranspose.Algorithm,
		},
		resp: response{
			rowSchema:  rowSchema,
			mediaType:  m.Response.MediaType,
			override:   m.Response.OverrideMediaType,
			objectKey:  m.Response.ObjectKey,
			transform:  transformDecl(m.Response.Transform),
			pagination: d.pagination(m, op, schema, verb),
			schema:     schema,
		},
	}
}

// pagination is how a method's results continue past the first page, as the document states it:
// the method's own declaration (or its resource's); else the document's; else native
// x-ms-pageable metadata (tagged so hosts can opt into legacy pagination); else
// the pageToken / nextPageToken pair a list method visibly declares (Google's), or AWS's Marker /
// NextToken. Where the document states nothing, any-sdk's defaults apply, by provider name as any-sdk
// applies them (any-sdk public/providerinvokers/anysdkhttp/invoker.go, inferNextPageResponseElement
// and inferNextPageRequestElement): GitHub and Okta follow the Link header; every other provider
// sends a body nextPageToken back as the pageToken query parameter. A reply without the token is the
// last page, so a method that does not page costs nothing.
func (d *document) pagination(m method, op pathOp, schema aot.Schema, verb string) pagination {
	if p := (pagination{req: m.Config.Pagination.RequestToken, resp: m.Config.Pagination.ResponseToken, algorithm: m.Config.Pagination.Algorithm, terminator: m.Config.Pagination.ResponseTerminator}); p.Declared() {
		return p
	}
	if p := (pagination{req: d.Config.Pagination.RequestToken, resp: d.Config.Pagination.ResponseToken, algorithm: d.Config.Pagination.Algorithm, terminator: d.Config.Pagination.ResponseTerminator}); p.Declared() {
		return p
	}
	if op.MsPageable != nil && op.MsPageable.NextLinkName != nil && *op.MsPageable.NextLinkName != "" {
		return pagination{native: true, req: tokenSpec{Location: "request"}, resp: tokenSpec{Key: *op.MsPageable.NextLinkName, Location: "body"}}
	}
	for _, p := range op.Parameters {
		switch {
		case p.Name == "pageToken" && p.In == "query" && schema != nil:
			if _, ok := schema.Property("nextPageToken"); ok {
				return pagination{req: tokenSpec{Key: "pageToken", Location: "query"}, resp: tokenSpec{Key: "nextPageToken", Location: "body"}}
			}
		case p.Name == "Marker" || p.Name == "NextToken":
			// AWS's query protocol pages by a Marker or NextToken parameter, echoed under the same
			// name in the reply beside the rows (EC2 writes it nextToken). A reply without it is the
			// last page, so a parameter that turns out not to page costs nothing.
			loc := p.In
			if loc == "" {
				loc = "query"
			}
			return pagination{req: tokenSpec{Key: p.Name, Location: loc}, resp: tokenSpec{Key: p.Name, Location: "body"}}
		}
	}
	// The defaults page reads only: a request with an effect, repeated for a next page, would repeat
	// the effect. verb is the HTTP method.
	if !strings.EqualFold(verb, "get") {
		return pagination{}
	}
	switch d.providerName {
	case "github", "okta":
		return pagination{req: tokenSpec{Location: "request"}, resp: tokenSpec{Key: "Link", Location: "header"}}
	}
	return pagination{req: tokenSpec{Key: "pageToken", Location: "query"}, resp: tokenSpec{Key: "nextPageToken", Location: "body"}}
}

// bodyMediaType is how the operation writes its request body, empty where it declares none. A
// document states one content type per body; where it somehow states several, the first in sorted
// order is taken so the same document always compiles the same way.
func bodyMediaType(op pathOp) string {
	types := make([]string, 0, len(op.RequestBody.Content))
	for mt := range op.RequestBody.Content {
		types = append(types, mt)
	}
	if len(types) == 0 {
		return ""
	}
	sort.Strings(types)
	return types[0]
}

// responseSchema resolves an operation's declared success shape. A schema-driven transform has no
// other instructions, so this is what makes one runnable — and its absence is why a document naming
// such a transform silently yields nothing.
func (d *document) responseSchema(op pathOp, key string) aot.Schema {
	return d.responseSchemaAt(op, key, "", "")
}
func (d *document) responseSchemaAt(op pathOp, key, selection, mediaType string, overrides ...*yaml.Node) aot.Schema {
	components := map[string]*yaml.Node{}
	for name := range d.Components.Schemas {
		node := d.Components.Schemas[name]
		components[name] = &node
	}
	if len(overrides) > 0 && overrides[0] != nil && overrides[0].Kind != 0 {
		return schemaAtSelection(overrides[0], components, selection)
	}
	// The method names the response it reads; failing that, the first success response. A range
	// (2XX) counts, as OpenAPI allows.
	codes := []string{"200", "201", "202", "204", "2XX", "2xx", "default"}
	if key != "" {
		codes = append([]string{key}, codes...)
	}
	for _, code := range codes {
		resp, ok := op.Responses[code]
		if !ok {
			continue
		}
		for hops := 0; resp.Ref != "" && hops < 8; hops++ {
			if resp, ok = d.Components.Responses[lastSegment(resp.Ref)]; !ok {
				break
			}
		}
		if !ok {
			continue
		}
		// An operation can expose different row shapes for different media types.
		// Respect the method declaration; any fallback must also be deterministic.
		mediaTypes := slices.Sorted(maps.Keys(resp.Content))
		if _, present := resp.Content[mediaType]; present {
			mediaTypes = append([]string{mediaType}, mediaTypes...)
		}
		for _, selected := range mediaTypes {
			node := resp.Content[selected].Schema
			if node.Kind == 0 {
				continue
			}
			return schemaAtSelection(&node, components, selection)
		}
	}
	return nil
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

// A URL scheme is transport configuration. Defaults for every other server
// variable remain scope, even when the document supplies a default value.
func serverTransportDefaults(s server) map[string]string {
	scheme, _, ok := strings.Cut(serverURL(s.URL), "://")
	if !ok || !strings.HasPrefix(scheme, "{") || !strings.HasSuffix(scheme, "}") {
		return nil
	}
	name := strings.TrimSuffix(strings.TrimPrefix(scheme, "{"), "}")
	value := s.Variables[name].Default
	if value != "http" && value != "https" {
		return nil
	}
	return map[string]string{name: value}
}

// serverVars are the server URL's {variables}, which become bound inputs — a region is a scope
// decision the caller makes, never something a parser invents. Sorted for a stable signature.
func serverVars(s server) []string {
	out := make([]string, 0, len(s.Variables))
	u := serverURL(s.URL)
	for k := range s.Variables {
		if strings.Contains(u, "{"+k+"}") {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// serverURL reduces a server URL's {name:pattern} variables to {name}. stackql documents constrain a
// variable with a regular expression written inside its braces (k8s: {cluster_addr:^…$}); left in,
// the placeholder names nothing and the variable is never bound. The pattern may itself contain
// braces, so the variable ends at the brace that closes it, not the first one.
func serverURL(u string) string {
	var b strings.Builder
	for i := 0; i < len(u); i++ {
		if u[i] != '{' {
			b.WriteByte(u[i])
			continue
		}
		end := i + 1
		for end < len(u) && u[end] != ':' && u[end] != '}' {
			end++
		}
		if end >= len(u) || u[end] == '}' {
			b.WriteByte(u[i])
			continue
		}
		depth, j := 1, end+1
		for ; j < len(u) && depth > 0; j++ {
			switch u[j] {
			case '{':
				depth++
			case '}':
				depth--
			}
		}
		b.WriteString(u[i:end] + "}")
		i = j - 1
	}
	return b.String()
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
	return unescapePointer(rest[:i]), strings.ToLower(rest[i+1:]), nil
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
	fingerprint   string
	inverseRef    string
	inverseTokens map[string][3]string
	inline        []string
	retry         aot.HTTPRetry
	graphQL       aot.GraphQL
	name          string
	sec           security
	inputs        []string
	opID          string
	req           request
	resp          response
	beta          aot.Beta
	pushdown      *pushdownSpec
}

func (e *aotExchange) DocumentIdentity() string { return e.fingerprint }

func (e *aotExchange) InverseDeclaration() (string, map[string][3]string) {
	return e.inverseRef, maps.Clone(e.inverseTokens)
}
func (e *aotExchange) InlineCommand() []string { return slices.Clone(e.inline) }

func (e *aotExchange) Retry() (aot.HTTPRetry, bool) { return e.retry, e.retry != nil }

func (e *aotExchange) GraphQL() (aot.GraphQL, bool) { return e.graphQL, e.graphQL != nil }

func (e *aotExchange) Name() string           { return e.name }
func (e *aotExchange) Inputs() []string       { return e.inputs }
func (e *aotExchange) OperationID() string    { return e.opID }
func (e *aotExchange) Request() aot.Request   { return e.req }
func (e *aotExchange) Response() aot.Response { return e.resp }
func (e *aotExchange) Security() aot.Security { return e.sec }
func (e *aotExchange) Beta() (aot.Beta, bool) { return e.beta, e.beta != nil }

// Pushdown is nil-safe: an exchange with no declaration offers none, rather than a nil option set.
func (e *aotExchange) Pushdown() (aot.Pushdown, bool) {
	if e.pushdown == nil {
		return nil, false
	}
	return e.pushdown, true
}

// operationParams are the operation's declared inputs. The __-prefixed markers are excluded: they are
// the operation's identity lifted out of the path key, already placed as fixed body params, and are
// not something a caller supplies.
func (d *document) operationParams(op pathOp) []aot.Parameter {
	out := make([]aot.Parameter, 0, len(op.Parameters))
	for _, p := range op.Parameters {
		if p.Name == "" || strings.HasPrefix(p.Name, "__") {
			continue
		}
		out = append(out, parameter{p: p, list: d.isArray(p)})
	}
	return out
}

// isArray reports whether a parameter's schema, followed through a components $ref, is an array.
func (d *document) isArray(p pathParam) bool {
	if p.Schema.Type == "array" {
		return true
	}
	const prefix = "#/components/schemas/"
	if !strings.HasPrefix(p.Schema.Ref, prefix) {
		return false
	}
	node, ok := d.Components.Schemas[strings.TrimPrefix(p.Schema.Ref, prefix)]
	if !ok {
		return false
	}
	var s struct {
		Type string `yaml:"type"`
	}
	return node.Decode(&s) == nil && s.Type == "array"
}

type parameter struct {
	p    pathParam
	list bool
}

func (p parameter) Name() string { return p.p.Name }
func (p parameter) In() string   { return p.p.In }
func (p parameter) Required() bool {
	return p.p.Required && !(p.p.In == aot.InHeader && p.p.Schema.Default != nil)
}
func (p parameter) List() bool   { return p.list }
func (p parameter) Default() any { return p.p.Schema.Default }

type request struct {
	requestTransform  transformDeclType
	xmlRootName       string
	xmlDeclaration    string
	xmlAnnotation     string
	bodyTranslation   string
	signingService    string
	defaultBody       string
	serverVariables   []string
	serverEnvironment map[string]string
	transportDefaults map[string]string
	baseBody          string
	nativeCasing      string
	snakeAliases      bool
	bodySchema        aot.Schema
	method            string
	url               string
	mediaType         string
	params            map[string]string
	parameters        []aot.Parameter
	bodyMediaType     string
	translate         string
	transpose         string
}

func (r request) XMLRootName() string       { return r.xmlRootName }
func (r request) XMLDeclaration() string    { return r.xmlDeclaration }
func (r request) XMLRootAnnotation() string { return r.xmlAnnotation }

func (r request) SigningService() string { return r.signingService }

func (r request) BodyTranslation() string { return r.bodyTranslation }

func (r request) DefaultBody() string { return r.defaultBody }

func (r request) ServerEnvironmentBindings() map[string]string {
	return maps.Clone(r.serverEnvironment)
}

func (r request) ServerTransportDefaults() map[string]string { return maps.Clone(r.transportDefaults) }

func (r request) ServerVariables() []string { return slices.Clone(r.serverVariables) }

func (r request) BaseBody() string { return r.baseBody }

func (r request) NativeCasing() string   { return r.nativeCasing }
func (r request) SnakeCaseAliases() bool { return r.snakeAliases }
func (r request) BodySchema() aot.Schema { return r.bodySchema }

func (r request) Parameters() []aot.Parameter { return r.parameters }
func (r request) BodyMediaType() string       { return r.bodyMediaType }
func (r request) Translate() string           { return r.translate }
func (r request) Transpose() string           { return r.transpose }

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
	rowSchema  aot.Schema
	mediaType  string
	override   string
	objectKey  string
	transform  transformDeclType
	pagination pagination
	schema     aot.Schema
}

func (r response) MediaType() string          { return r.mediaType }
func (r response) OverrideMediaType() string  { return r.override }
func (r response) ObjectKey() string          { return r.objectKey }
func (r response) Transform() aot.Transform   { return r.transform }
func (r response) Pagination() aot.Pagination { return r.pagination }
func (r response) Schema() aot.Schema         { return r.schema }

type transformDeclType struct{ typ, body string }

func transformDecl(t transformSpec) transformDeclType {
	return transformDeclType{typ: t.Type, body: t.Body}
}

func (t transformDeclType) Type() string { return t.typ }
func (t transformDeclType) Body() string { return t.body }

type pagination struct {
	native     bool
	req, resp  tokenSpec
	algorithm  string
	terminator tokenSpec
}

func (p pagination) Algorithm() string { return p.algorithm }
func (p pagination) Native() bool      { return p.native }
func (p pagination) ResponseTerminator() (string, string) {
	return p.terminator.Key, p.terminator.Location
}

func (p pagination) RequestToken() (string, string)  { return p.req.Key, p.req.Location }
func (p pagination) RequestTokenEncoding() string    { return p.req.Encoding }
func (p pagination) ResponseToken() (string, string) { return p.resp.Key, p.resp.Location }
func (p pagination) Declared() bool                  { return p.algorithm != "" || p.req.Key != "" || p.resp.Key != "" }

// pushdownSpec is a queryParamPushdown declaration: the server-side query options a call offers, as
// any-sdk reads them.
type pushdownSpec struct {
	OrderBySpec *pushdownOption `yaml:"orderBy"`
	SkipSpec    *pushdownOption `yaml:"skip"`
	CountSpec   *pushdownOption `yaml:"count"`
	SelectSpec  *pushdownOption `yaml:"select"`
	FilterSpec  *pushdownOption `yaml:"filter"`
	TopSpec     *pushdownOption `yaml:"top"`
}

func (p *pushdownSpec) OrderBy() (aot.PushdownOption, bool) { return p.OrderBySpec.opt("$orderby") }
func (p *pushdownSpec) Skip() (aot.PushdownOption, bool)    { return p.SkipSpec.opt("$skip") }
func (p *pushdownSpec) Count() (aot.PushdownOption, bool)   { return p.CountSpec.opt("$count") }

func (p *pushdownSpec) Select() (aot.PushdownOption, bool) { return p.SelectSpec.opt("$select") }
func (p *pushdownSpec) Filter() (aot.PushdownOption, bool) { return p.FilterSpec.opt("$filter") }
func (p *pushdownSpec) Top() (aot.PushdownOption, bool)    { return p.TopSpec.opt("$top") }

// opt is the option, with odata's parameter name where the document names none.
func (o *pushdownOption) opt(odataParam string) (aot.PushdownOption, bool) {
	if o == nil {
		return nil, false
	}
	c := *o
	if c.DialectName == "" {
		c.DialectName = "custom"
	}
	if c.Param == "" && c.DialectName == "odata" {
		c.Param = odataParam
	}
	if c.Delim == "" {
		c.Delim = ","
	}
	return c, true
}

type pushdownOption struct {
	SyntaxName         string   `yaml:"syntax"`
	DialectName        string   `yaml:"dialect"`
	Param              string   `yaml:"paramName"`
	Delim              string   `yaml:"delimiter"`
	Max                int      `yaml:"maxValue"`
	SupportedColumns   []string `yaml:"supportedColumns"`
	SupportedOperators []string `yaml:"supportedOperators"`
}

func (o pushdownOption) Dialect() string   { return o.DialectName }
func (o pushdownOption) ParamName() string { return o.Param }
func (o pushdownOption) Delimiter() string { return o.Delim }
func (o pushdownOption) MaxValue() int     { return o.Max }

// SupportsColumn and SupportsOperator: a list the document leaves empty, or one naming "*", allows
// anything.
func (o pushdownOption) SupportsColumn(name string) bool { return supported(name, o.SupportedColumns) }
func (o pushdownOption) SupportsOperator(op string) bool { return supported(op, o.SupportedOperators) }

func supported(item string, list []string) bool {
	return len(list) == 0 || slices.Contains(list, "*") || slices.Contains(list, item)
}

// defaultMethodKeys retains legacy registry matching only when a verb has no
// explicit mapping. An explicit empty mapping disables that SQL verb.
func defaultMethodKeys(verb string) []string {
	switch verb {
	case "select":
		return []string{"select", "list", "aggregatedList", "get"}
	case "insert":
		return []string{"insert", "create"}
	case "delete":
		return []string{"delete"}
	}
	return nil
}

func (r response) RowSchema() aot.Schema { return r.rowSchema }

func (o pushdownOption) Syntax() string {
	if o.SyntaxName != "" {
		return o.SyntaxName
	}
	return "odata"
}
func (o pushdownOption) FixedColumn() string {
	if len(o.SupportedColumns) == 1 && o.SupportedColumns[0] != "*" {
		return o.SupportedColumns[0]
	}
	return ""
}

func (r request) RequestTransform() aot.Transform { return r.requestTransform }

// ResourceNames preserves declared presentation names separately from routing keys.
func (d *document) ResourceNames() map[string]string {
	names := make(map[string]string, len(d.Components.Resources))
	for key, resource := range d.Components.Resources {
		names[key] = resource.Name
		if names[key] == "" {
			names[key] = key
		}
	}
	return names
}

func (p parameter) Type() string { return p.p.Schema.Type }
