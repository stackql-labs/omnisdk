// Package docsem derives resource semantics from provider documents.
//
// Most of what an IaC run needs about a resource is already stated in the document: which operation
// creates it, which deletes it, which reads it. Deriving those removes the hand-authored table that
// is otherwise written once per provider — the difference between semantics as data and semantics as
// code.
//
// It sits apart from the semantics package so that package stays a leaf on facade alone: this one
// depends on the document contract as well, and the direction only runs one way.
package docsem

import (
	"fmt"

	"github.com/stackql-labs/omnisdk/internal/semantics"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
)

// Resource is one derived resource: the operations that act on it, named as the document names them.
type Resource interface {
	// Path is the document's address, e.g. "aws.ec2.vpcs".
	Path() string
	// Create, Read, Update and Delete are operation ids, empty where the document declares none.
	Create() string
	Read() string
	Update() string
	Delete() string
}

type resource struct {
	path                          string
	create, read, update, deleteX string
}

func (r resource) Path() string   { return r.path }
func (r resource) Create() string { return r.create }
func (r resource) Read() string   { return r.read }
func (r resource) Update() string { return r.update }
func (r resource) Delete() string { return r.deleteX }

// Derive reads what the document states about each named resource. Passing no names sweeps the whole
// service, which for a large provider means parsing every resource it declares — name the ones you
// need unless you actually want the sweep.
func Derive(cat aot.Catalog, service string, names ...string) ([]Resource, error) {
	if len(names) == 0 {
		all, err := cat.Resources(service)
		if err != nil {
			return nil, fmt.Errorf("docsem: resources of %s: %w", service, err)
		}
		names = all
	}
	provider := cat.Provider().Name()
	out := make([]Resource, 0, len(names))
	for _, name := range names {
		methods, err := cat.Methods(service, name)
		if err != nil {
			return nil, fmt.Errorf("docsem: methods of %s.%s: %w", service, name, err)
		}
		r := resource{path: provider + "." + service + "." + name}
		for _, m := range methods {
			// First declaration of a verb wins. That is right for a resource with one operation per
			// verb and wrong where a document binds several — ec2.subnets declares both
			// CreateSubnet and CreateDefaultSubnet, and only the caller's inputs decide which is
			// meant. Candidates, not conclusions: a caller that knows better names the operation.
			switch semantics.FormOf(m.SQLVerb()) {
			case facade.FormCreate:
				if r.create == "" {
					r.create = m.OperationID()
				}
			case facade.FormUpdate:
				if r.update == "" {
					r.update = m.OperationID()
				}
			case facade.FormDelete:
				if r.deleteX == "" {
					r.deleteX = m.OperationID()
				}
			case facade.FormRead:
				// FormOf defaults unknown verbs to read, so only an explicit select counts.
				if m.SQLVerb() == "select" && r.read == "" {
					r.read = m.OperationID()
				}
			}
		}
		out = append(out, r)
	}
	return out, nil
}

// Declarations turns derived resources into semantics declarations.
//
// The inverse of a create is the same resource's delete, and its absence is what declares the create
// non-invertible — no hand-authored link. Fidelity is not derivable: a document does not say whether
// a delete loses anything, so every derived inverse is lossy until something states otherwise, which
// errs toward asking rather than assuming.
//
// The update path is deliberately NOT derived. A document maps several unrelated operations to the
// update verb — AssociateSecurityGroupVpc is an update of a VPC in SQL terms and nothing like a
// convergence of its own fields — and dispatching drift to one of those would apply a change nobody
// asked for. An absent update path refuses drift instead, which is the safe direction.
func Declarations(resources []Resource) []semantics.Declaration {
	var out []semantics.Declaration
	for _, r := range resources {
		if r.Create() == "" {
			continue
		}
		out = append(out, semantics.Declaration{
			Exchange: r.Create(),
			Form:     "insert",
			Inverse:  r.Delete(),
		})
	}
	return out
}

// Select resolves one verb on one resource to a single operation, by the document's own rule: scan
// the declarations in order and take the first whose signature the supplied inputs satisfy.
//
// A verb fans out — ec2.subnets declares both CreateSubnet and CreateDefaultSubnet under insert —
// and the verb alone cannot say which is meant. The inputs can: only CreateSubnet accepts VpcId and
// CidrBlock. Order breaks ties between operations that both match.
func Select(cat aot.Catalog, path, verb string, inputs []string) (aot.AOTExchange, error) {
	ops, err := cat.Operations(path, verb)
	if err != nil {
		return nil, fmt.Errorf("docsem: %s %s: %w", path, verb, err)
	}
	supplied := make(map[string]bool, len(inputs))
	for _, in := range inputs {
		supplied[in] = true
	}
	for _, op := range ops {
		if accepts(op, supplied) {
			return op, nil
		}
	}
	return nil, fmt.Errorf("docsem: %s %s: no operation accepts %v", path, verb, inputs)
}

// accepts reports whether an operation declares somewhere for every supplied input to go. An input
// the operation does not know is what rules it out — sending it would either be dropped silently or
// rejected, and both are worse than choosing the operation that declares it.
//
// Inputs count alongside parameters: a server variable such as a region is supplied by the caller
// exactly as a parameter is, and it is not in the parameter list.
func accepts(op aot.AOTExchange, supplied map[string]bool) bool {
	declared := map[string]bool{}
	for _, in := range op.Inputs() {
		declared[in] = true
	}
	for _, p := range op.Request().Parameters() {
		declared[p.Name()] = true
	}
	for in := range supplied {
		if !declared[in] {
			return false
		}
	}
	return true
}

// Outlier is an operation a document describes incompletely. It is reported rather than tolerated
// silently: the engine can run such an operation — an undocumented response is assumed empty and
// answered with the status code — but a caller deciding whether a resource is safely manageable
// needs to know which of its operations the document does not fully describe.
type Outlier interface {
	// Path is the resource address.
	Path() string
	// Verb is the SQL verb the operation is bound to.
	Verb() string
	// Operation is the document's own id for it.
	Operation() string
	// Reason says what the document leaves unstated.
	Reason() string
}

type outlier struct{ path, verb, op, reason string }

func (o outlier) Path() string      { return o.path }
func (o outlier) Verb() string      { return o.verb }
func (o outlier) Operation() string { return o.op }
func (o outlier) Reason() string    { return o.reason }

// Outliers reports every operation of a resource whose response the document does not describe well
// enough to read. Verbs are examined in turn; a verb the resource does not declare is not an
// outlier, it is simply absent.
func Outliers(cat aot.Catalog, path string, verbs ...string) ([]Outlier, error) {
	if len(verbs) == 0 {
		verbs = []string{"select", "insert", "update", "delete"}
	}
	var out []Outlier
	for _, verb := range verbs {
		ops, err := cat.Operations(path, verb)
		if err != nil {
			// A verb the resource does not bind is not a finding.
			continue
		}
		for _, op := range ops {
			resp := op.Response()
			switch {
			case resp == nil:
				out = append(out, outlier{path, verb, op.OperationID(), "declares no response"})
			case resp.MediaType() == "" && resp.OverrideMediaType() == "":
				out = append(out, outlier{path, verb, op.OperationID(), "declares no response media type, so the body cannot be decoded"})
			case resp.ObjectKey() == "" && verb == "select":
				out = append(out, outlier{path, verb, op.OperationID(), "declares no objectKey, so the item list cannot be located"})
			}
		}
	}
	return out, nil
}
