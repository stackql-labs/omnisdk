package omnisdk

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/stackql-labs/omnisdk/internal/system_g/bind"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/plan"
)

// Relation is a read handle resolved by a caller's analyzer. Its schema and
// inputs participate in normal query resolution; Open owns the execution of
// the underlying query or storage read. Each successful Open transfers cursor
// ownership to Omni. Implementations must support concurrent independent opens.
type Relation interface {
	Table
	Open(context.Context, map[string]any) (Rows, error)
}

// NewRelation declares one read signature without performing IO. The opener
// receives only declared input values, including values arriving from joins.
// It must honor cancellation and release its resources when Rows.Close runs.
func NewRelation(address string, params []Param, columns map[string]ColumnType,
	open func(context.Context, map[string]any) (Rows, error),
) (Relation, error) {
	if address == "" || open == nil {
		return nil, fmt.Errorf("omnisdk: relation requires an address and opener")
	}
	seen := map[string]bool{}
	for _, p := range params {
		if p.Name == "" || seen[p.Name] {
			return nil, fmt.Errorf("omnisdk: relation %s has an empty or duplicate input %q", address, p.Name)
		}
		seen[p.Name] = true
	}
	if _, invalid := columns[""]; invalid {
		return nil, fmt.Errorf("omnisdk: relation %s has an empty column name", address)
	}
	return &readRelation{address: address, params: slices.Clone(params), columns: maps.Clone(columns), open: open}, nil
}

type readRelation struct {
	address string
	params  []Param
	columns map[string]ColumnType
	open    func(context.Context, map[string]any) (Rows, error)
}

func (r *readRelation) Address() string            { return r.address }
func (r *readRelation) Methods() []MethodSignature { return []MethodSignature{relationSignature{r}} }
func (r *readRelation) Open(ctx context.Context, inputs map[string]any) (Rows, error) {
	return r.open(ctx, inputs)
}

type relationSignature struct{ relation *readRelation }

func (s relationSignature) Method() string                     { return "select" }
func (s relationSignature) TakesBody() bool                    { return false }
func (s relationSignature) Columns() []string                  { return slices.Sorted(maps.Keys(s.relation.columns)) }
func (s relationSignature) ColumnTypes() map[string]ColumnType { return maps.Clone(s.relation.columns) }
func (s relationSignature) Params() []ParamSignature {
	result := make([]ParamSignature, len(s.relation.params))
	for i, p := range s.relation.params {
		result[i] = relationParam{p}
	}
	return result
}

type relationParam struct{ param Param }

func (p relationParam) Name() string   { return p.param.Name }
func (p relationParam) Required() bool { return p.param.Required }
func (p relationParam) In() string     { return "relation" }

func relationSpec(name string, relation Relation, arrivals map[string]any, wired []string) (plan.ExchangeSpec, []string, error) {
	methods := relation.Methods()
	if len(methods) != 1 || methods[0].Method() != "select" {
		return nil, nil, fmt.Errorf("omnisdk: relation %s must declare one select signature", relation.Address())
	}
	var inputs, required []string
	for _, p := range methods[0].Params() {
		if p.Required() {
			if _, present := arrivals[p.Name()]; !present {
				return nil, nil, fmt.Errorf("omnisdk: relation %s requires %s", relation.Address(), p.Name())
			}
			required = append(required, p.Name())
		}
		if _, present := arrivals[p.Name()]; present {
			inputs = append(inputs, p.Name())
		}
	}
	var source facade.RelationSource = relationSource{relation: relation, inputs: inputs}
	inbox := slices.Clone(inputs)
	for _, name := range wired {
		if !slices.Contains(inbox, name) {
			inbox = append(inbox, name)
		}
	}
	return plan.NewExchangeSpec(name, inbox, methods[0].Columns(), source.Make, bind.NewInnerFlatten()), required, nil
}

type relationSource struct {
	relation Relation
	inputs   []string
}

func (s relationSource) Make(bound map[string]any) facade.Operator {
	inputs := make(map[string]any, len(s.inputs))
	for _, name := range s.inputs {
		if v, present := bound[name]; present {
			inputs[name] = v
		}
	}
	return relationOperator{relation: s.relation, inputs: inputs}
}

type relationOperator struct {
	relation Relation
	inputs   map[string]any
}

func (o relationOperator) Open(ctx context.Context) facade.Records {
	if err := ctx.Err(); err != nil {
		return &relationRecords{err: err}
	}
	if err := facade.CheckAccess(ctx, "relation/"+o.relation.Address(), "read"); err != nil {
		return &relationRecords{err: err}
	}
	rows, err := o.relation.Open(ctx, maps.Clone(o.inputs))
	if err == nil && rows == nil {
		err = fmt.Errorf("omnisdk: relation %s returned a nil cursor", o.relation.Address())
	}
	return &relationRecords{rows: rows, err: err}
}

type relationRecords struct {
	rows    Rows
	err     error
	current facade.Record
}

func (r *relationRecords) Next(ctx context.Context) bool {
	if r.err != nil || r.rows == nil {
		return false
	}
	if err := ctx.Err(); err != nil {
		r.err = err
		return false
	}
	if !r.rows.Next() {
		r.err = r.rows.Err()
		return false
	}
	r.current = bind.NewDocRecord(maps.Clone(r.rows.Row()))
	return true
}
func (r *relationRecords) Record() facade.Record { return r.current }
func (r *relationRecords) Err() error            { return r.err }
func (r *relationRecords) Close() error {
	if r.rows == nil {
		return nil
	}
	rows := r.rows
	r.rows = nil
	return rows.Close()
}
