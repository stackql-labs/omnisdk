package docx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os/exec"
	"text/template"

	"github.com/stackql-labs/omnisdk/internal/system_g/bind"
	"github.com/stackql-labs/omnisdk/internal/system_g/exchange"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/plan"
	"github.com/stackql-labs/omnisdk/internal/system_g/requestpolicy"
	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
	"github.com/stackql-labs/omnisdk/pkg/docparse/dsl"
)

func localSpec(ex aot.AOTExchange, argv []string, inputs map[string]any, reg dsl.Registry, o options) (plan.ExchangeSpec, error) {
	var in []string
	for _, p := range ex.Request().Parameters() {
		if p.Required() || o.bound[p.Name()] {
			in = append(in, p.Name())
		}
	}
	shape := docLocal{ex: ex, argv: argv, inputs: inputs, reg: reg}
	return plan.NewExchangeSpec(ex.Name(), in, nil, func(bound map[string]any) facade.Operator {
		values := maps.Clone(inputs)
		if values == nil {
			values = map[string]any{}
		}
		maps.Copy(values, bound)
		op := shape
		op.inputs = values
		return op
	}, nil), nil
}

type docLocal struct {
	ex     aot.AOTExchange
	argv   []string
	inputs map[string]any
	reg    dsl.Registry
}

func (o docLocal) Open(ctx context.Context) facade.Records {
	return &localRecords{ctx: ctx, op: o}
}

type localRecords struct {
	ctx     context.Context
	op      docLocal
	rows    facade.Records
	err     error
	started bool
}

func (r *localRecords) Next(ctx context.Context) bool {
	if !r.started {
		r.started = true
		r.rows, r.err = r.op.run(r.ctx)
	}
	return r.err == nil && r.rows != nil && r.rows.Next(ctx)
}
func (r *localRecords) Record() facade.Record { return r.rows.Record() }
func (r *localRecords) Err() error {
	if r.err != nil {
		return r.err
	}
	if r.rows != nil {
		return r.rows.Err()
	}
	return nil
}
func (r *localRecords) Close() error {
	if r.rows != nil {
		return r.rows.Close()
	}
	return nil
}
func (o docLocal) run(ctx context.Context) (facade.Records, error) {
	commandCtx, cancel := requestpolicy.Context(ctx)
	defer cancel()
	argv := make([]string, len(o.argv))
	for i, text := range o.argv {
		t, err := template.New("argv").Parse(text)
		if err != nil {
			return nil, err
		}
		var b bytes.Buffer
		if err = t.Execute(&b, map[string]any{"parameters": o.inputs}); err != nil {
			return nil, err
		}
		argv[i] = b.String()
	}
	cmd := exec.CommandContext(commandCtx, argv[0], argv[1:]...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("local command: %w: %s", err, stderr.String())
	}
	response := o.ex.Response()
	tr := response.Transform()
	var value any = map[string]any{}
	if tr.Type() != "" && tr.Body() != "" {
		output, err = o.reg.Eval(tr.Type(), dsl.Context{}, tr.Body(), output)
		if err != nil {
			return nil, err
		}
		decoder := json.NewDecoder(bytes.NewReader(output))
		decoder.UseNumber()
		if err = decoder.Decode(&value); err != nil {
			return nil, err
		}
	} else if len(bytes.TrimSpace(output)) > 0 {
		if err = json.Unmarshal(output, &value); err != nil {
			value = map[string]any{"stdout": string(output)}
		}
	}
	list, ok := value.([]any)
	if !ok {
		list = []any{value}
	}
	records := make([]facade.Record, 0, len(list))
	for _, v := range list {
		if row, ok := v.(map[string]any); ok {
			records = append(records, bind.NewDocRecord(row))
		}
	}
	return exchange.NewLiteralSource(records, 1).Open(ctx), nil
}
