// Package docrun is the document-driven effector: it performs IaC effects for ANY provider whose
// document the registry holds, by compiling the declared operation and running it.
//
// Nothing here is per-provider. The hand-written action tables it replaces existed only because the
// parser exposed SELECT operations and nothing else; with a verb-agnostic lookup, a create is
// resolved exactly as a read always was, and adding a provider is adding a document.
//
// Two things a document does not state, and which therefore arrive as data rather than code: which
// response field carries the object's identity, and which parameter takes the correlation stamp.
// Those are the residue — per-resource, not per-resource-code.
package docrun

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/stackql-labs/omnisdk/internal/docsem"
	"github.com/stackql-labs/omnisdk/internal/system_g/bind"
	"github.com/stackql-labs/omnisdk/internal/system_g/exchange/docx"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/plan"
	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
	"github.com/stackql-labs/omnisdk/pkg/docparse/dsl"
)

// Residue is what a provider document does not state about a resource. It is data supplied
// alongside the document, never code: the whole point is that a new resource is a declaration.
type Residue interface {
	// Identity is the path to the response field carrying the object's address after a create,
	// dotted, e.g. "CreateVpcResponse.vpc.vpcId". A path rather than a name because the reply is the
	// document's own shape and nothing flattens it.
	Identity() string
	// AddressedBy names the request parameter that carries an existing object's identity, so a
	// delete or a read can be built from the ledger entry alone.
	AddressedBy() string
	// CorrelationParam names the parameter that takes the correlation stamp, empty where the
	// provider offers no writable, searchable field — in which case the log is the sole link to
	// reality and losing it orphans the resource.
	CorrelationParam() string
}

type residue struct{ identity, addressedBy, correlation string }

func (r residue) Identity() string         { return r.identity }
func (r residue) AddressedBy() string      { return r.addressedBy }
func (r residue) CorrelationParam() string { return r.correlation }

// NewResidue declares what a document leaves unsaid about one resource.
func NewResidue(identity, addressedBy, correlationParam string) Residue {
	return residue{identity: identity, addressedBy: addressedBy, correlation: correlationParam}
}

// Effector performs effects against documents.
type effector struct {
	catalog aot.Catalog
	dsl     dsl.Registry
	residue map[string]Residue // resource address → residue
	opts    []docx.Option
}

// New returns an Effector over one provider's catalog. opts carry credentials and any endpoint
// override, exactly as a query would supply them.
func New(cat aot.Catalog, dslReg dsl.Registry, residues map[string]Residue, opts ...docx.Option) facade.Effector {
	return &effector{catalog: cat, dsl: dslReg, residue: residues, opts: opts}
}

// address splits an exchange name of the form "<provider>.<service>.<resource>:<verb>". The verb is
// part of the name because one address fans out — a resource's insert and delete are two operations
// on the same thing.
func address(exchange string) (addr, verb string, err error) {
	addr, verb, ok := strings.Cut(exchange, ":")
	if !ok {
		return "", "", fmt.Errorf("docrun: exchange %q is not <address>:<verb>", exchange)
	}
	return addr, verb, nil
}

func (e *effector) Effect(ctx context.Context, exchange string, k facade.LedgerKey, in facade.EffectInput) ([]byte, error) {
	addr, verb, err := address(exchange)
	if err != nil {
		return nil, err
	}
	res := e.residue[addr]
	// Whether the operation takes a body decides where the intent goes: into the body whole, or
	// scattered across parameters. The document states it, so the decision is not a guess.
	bodied, err := e.takesBody(addr, verb)
	if err != nil {
		return nil, err
	}
	inputs, err := e.inputs(addr, k, in, res, verb, bodied)
	if err != nil {
		return nil, err
	}
	var body []byte
	if bodied {
		body = in.Mutation
	}
	row, err := e.run(ctx, addr, verb, inputs, body)
	if err != nil {
		return nil, fmt.Errorf("docrun: %s %s %s: %w", addr, verb, k, err)
	}
	if verb != "insert" || res == nil || res.Identity() == "" {
		// Only a create mints an address. An update or a delete answers about an object the caller
		// already holds the identity of, and reading a minted id out of that reply would be looking
		// for something that was never there.
		return in.Identity, nil
	}
	id, ok := at(row, res.Identity())
	if !ok || id == "" {
		return nil, fmt.Errorf("docrun: %s %s returned no %s", addr, k, res.Identity())
	}
	return []byte(id), nil
}

func (e *effector) Read(ctx context.Context, exchange string, k facade.LedgerKey, in facade.EffectInput) ([]byte, []byte, bool, error) {
	addr, _, err := address(exchange)
	if err != nil {
		return nil, nil, false, err
	}
	res := e.residue[addr]
	if res == nil || res.AddressedBy() == "" {
		// No declared way to address the object for a read: the log degrades from fact to belief,
		// which is a property of the target and is reported rather than hidden behind an empty
		// document that would read as "absent".
		return nil, nil, false, nil
	}
	if len(in.Identity) == 0 {
		// Identity unknown: without a correlation stamp there is no way to find the object again,
		// so absence cannot be distinguished from ignorance.
		return nil, nil, false, nil
	}
	// A read takes the address and the operation's own declared inputs — a server variable such as
	// a region — and nothing else. Passing the caller's parameters wholesale sends a create's
	// arguments to a describe, which the provider rejects outright.
	inputs := map[string]any{res.AddressedBy(): string(in.Identity)}
	scope, err := e.scopeOf(addr, "select")
	if err != nil {
		return nil, nil, false, err
	}
	for _, name := range scope {
		if v, given := in.Params[name]; given {
			inputs[name] = v
		}
	}
	// A read takes the whole response too: the caller is asking about one object, and the metadata
	// travels with it rather than being dropped on the way to a row.
	row, err := e.run(ctx, addr, "select", inputs, nil)
	if err != nil {
		return nil, nil, false, fmt.Errorf("docrun: read %s: %w", k, err)
	}
	if len(row) == 0 {
		return nil, nil, true, nil
	}
	identity := in.Identity
	if v, ok := at(row, res.Identity()); ok && v != "" {
		identity = []byte(v)
	}
	b, err := encodeRow(actualOf(row))
	if err != nil {
		return nil, nil, false, err
	}
	return b, identity, true, nil
}

// inputs assembles what the operation needs: the mutation's fields, the caller's parameters, an
// existing object's identity, and the correlation stamp where the resource declares somewhere to
// put it.
func (e *effector) inputs(addr string, k facade.LedgerKey, in facade.EffectInput, res Residue, verb string, bodied bool) (map[string]any, error) {
	inputs := map[string]any{}
	if len(in.Mutation) > 0 && verb != "delete" && !bodied {
		// Only where the operation takes no body: then the intent's fields ARE the parameters. An
		// operation that declares a body receives the document whole instead, because scattering its
		// fields into the query is how a create meant for a JSON API goes out as a query string.
		fields, err := decodeFields(in.Mutation)
		if err != nil {
			return nil, err
		}
		for name, v := range fields {
			if v == nil {
				// An unset has no expression in a request: a parameter is sent or it is not.
				continue
			}
			inputs[name] = v
		}
	}
	if res != nil {
		if res.AddressedBy() != "" && len(in.Identity) > 0 {
			inputs[res.AddressedBy()] = string(in.Identity)
		}
		if res.CorrelationParam() != "" && verb != "delete" {
			inputs[res.CorrelationParam()] = string(k)
		}
	}
	for name, v := range in.Params {
		inputs[name] = v
	}
	return inputs, nil
}

// run selects the operation by signature, compiles it, and drains the single row it yields.
// scopeOf collects the server variables the operations behind an address declare. These are the
// caller's to supply — a region is scope — and are the only parameters a read inherits.
func (e *effector) scopeOf(addr, verb string) ([]string, error) {
	ops, err := e.catalog.Operations(addr, verb)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, op := range ops {
		for _, in := range op.Inputs() {
			if !seen[in] {
				seen[in], out = true, append(out, in)
			}
		}
	}
	return out, nil
}

// at walks a dotted path into a decoded response and renders the leaf as text.
func at(doc map[string]any, path string) (string, bool) {
	if path == "" {
		return "", false
	}
	var cur any = doc
	for _, step := range strings.Split(path, ".") {
		// A projected response wraps its rows in a list even when the call concerns one object, so a
		// single-element list is stepped through transparently. More than one is ambiguous: the
		// caller asked about one object and the path cannot say which.
		if list, isList := cur.([]any); isList && len(list) == 1 {
			cur = list[0]
		}
		m, ok := cur.(map[string]any)
		if !ok {
			return "", false
		}
		cur, ok = m[step]
		if !ok {
			return "", false
		}
	}
	if list, isList := cur.([]any); isList && len(list) == 1 {
		cur = list[0]
	}
	if cur == nil {
		return "", false
	}
	return fmt.Sprint(cur), true
}

// takesBody reports whether the first operation matching an address and verb declares a request
// body. Resolving without inputs is enough: every operation for one verb on one resource agrees
// about whether it takes a body, since that is a property of the API's style rather than of the
// arguments.
func (e *effector) takesBody(addr, verb string) (bool, error) {
	ops, err := e.catalog.Operations(addr, verb)
	if err != nil || len(ops) == 0 {
		return false, nil
	}
	return ops[0].Request().BodyMediaType() != "", nil
}

func (e *effector) run(ctx context.Context, addr, verb string, inputs map[string]any, body []byte) (map[string]any, error) {
	names := make([]string, 0, len(inputs))
	for n := range inputs {
		names = append(names, n)
	}
	ex, err := docsem.Select(e.catalog, addr, verb, names)
	if err != nil {
		return nil, err
	}
	// Every operation here answers about ONE object, a read included: the address names it. So the
	// whole response is taken rather than an item list exploded out of it, and the metadata the call
	// reported arrives with it under docx.MetadataKey.
	//
	// WithoutResponseDecode covers the mutating case: a delete's reply is frequently undocumented,
	// and refusing to compile it would make a resource impossible to undo because of how its
	// response was written down.
	opts := append(append([]docx.Option{}, e.opts...), docx.WithWholeResponse(), docx.WithoutResponseDecode())
	// A list parameter is sent indexed — TagSpecification.1.Tag.2.Key, Filter.1.Name — and a document
	// declares the parameter, never its members. Placement matches declared names, so without this
	// every indexed member is dropped and the call goes out missing its tags or its filter.
	var indexed []string
	for name := range inputs {
		if strings.Contains(name, ".") {
			indexed = append(indexed, name)
		}
	}
	if len(body) > 0 {
		opts = append(opts, docx.WithBody(body))
	}
	if len(indexed) > 0 {
		// Bound, not merely placed: these values come from the caller, so they travel on the row and
		// the placeholder resolves. Placing without binding leaves the parameter on the wire with an
		// empty value, which a provider reads as a tag with no name.
		opts = append(opts, docx.WithBound(indexed...))
	}
	p, err := docx.PlanFor(ex, inputs, e.dsl, opts...)
	if err != nil {
		return nil, err
	}
	recs := plan.ComposeRows(1, p).Open(ctx)
	defer recs.Close()

	row := map[string]any{}
	for recs.Next(ctx) {
		doc, ok := bind.DocMap(recs.Record())
		if !ok {
			continue
		}
		for name, v := range doc {
			if v == nil {
				continue
			}
			// The plan carries its inputs on the row. They must not survive into what the caller
			// reads as ACTUAL state: convergence compares the intent against what the target holds,
			// and an echoed input would be the intent compared with itself, agreeing always.
			if _, echoed := inputs[name]; echoed {
				continue
			}
			row[name] = v
		}
	}
	if err := recs.Err(); err != nil {
		return nil, err
	}
	return row, nil
}

// decodeFields reads a mutation keeping numbers as their source text: through float64 an int64
// beyond 2^53 is rewritten on its way to the wire.
func decodeFields(b []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var fields map[string]any
	if err := dec.Decode(&fields); err != nil {
		return nil, fmt.Errorf("docrun: mutation is not a JSON object: %w", err)
	}
	return fields, nil
}

// actualOf reduces a response to the object's OWN state.
//
// A read answers about one object, but the projected response wraps it in the row list every reply
// shares. Handing that envelope back as "actual" puts the object's fields a level below where the
// desired document holds them, so convergence compares a field against nothing and reports drift on
// a resource that has never changed. The metadata travels alongside, since it belongs to the
// response rather than to the object.
func actualOf(row map[string]any) map[string]any {
	for _, v := range row {
		list, ok := v.([]any)
		if !ok || len(list) != 1 {
			continue
		}
		fields, ok := list[0].(map[string]any)
		if !ok {
			continue
		}
		out := make(map[string]any, len(fields)+1)
		for k, fv := range fields {
			out[k] = fv
		}
		if meta, has := row[docx.MetadataKey]; has {
			out[docx.MetadataKey] = meta
		}
		return out
	}
	return row
}

func encodeRow(row map[string]any) ([]byte, error) {
	b, err := json.Marshal(row)
	if err != nil {
		return nil, fmt.Errorf("docrun: encode actual: %w", err)
	}
	return b, nil
}
