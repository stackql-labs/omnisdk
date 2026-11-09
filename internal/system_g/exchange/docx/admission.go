package docx

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/stackql-labs/omnisdk/internal/system_g/bind"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
	"github.com/stackql-labs/omnisdk/pkg/docparse/dsl"
)

// InboundTransform validates the program and admission before any effect runs.
func InboundTransform(reg dsl.Registry, declaration dsl.Transform) (facade.Transform, error) {
	program := inboundProgram{reg: reg, typ: declaration.Type, body: declaration.Body, admission: declaration.Admission, required: declaration.Required}
	if err := program.Validate(); err != nil {
		return nil, err
	}
	return program, nil
}
func (p inboundProgram) Admission() facade.InboxAdmission {
	if p.admission == nil {
		return nil
	}
	return admissionProgram{reg: p.reg, program: *p.admission, required: p.required}
}

type admissionProgram struct {
	reg      dsl.Registry
	program  dsl.Program
	required []string
}

func (p admissionProgram) Admit(ctx context.Context, inbox map[string]any, edges map[string]facade.EdgeState) (bool, error) {
	if !requiredPresent(inbox, p.required) {
		return false, nil
	}
	input, err := json.Marshal(inbox)
	if err != nil {
		return false, err
	}
	output, err := p.reg.Eval(p.program.Type, dsl.Context{Execution: ctx, Edges: edgeDocuments(edges)}, p.program.Body, input)
	if err != nil {
		return false, fmt.Errorf("admission: %w", err)
	}
	var result any
	if err := json.Unmarshal(output, &result); err != nil {
		return false, err
	}
	admitted, ok := result.(bool)
	if !ok {
		return false, fmt.Errorf("admission must return a boolean")
	}
	return admitted, nil
}
func edgeDocuments(edges map[string]facade.EdgeState) map[string]map[string]any {
	out := make(map[string]map[string]any, len(edges))
	for name, edge := range edges {
		var failure any
		if edge.Error != "" {
			failure = edge.Error
		}
		out[name] = map[string]any{"present": edge.Present, "value": edge.Value, "done": edge.Done, "error": failure}
	}
	return out
}

// RequireAdmission preserves a document request's admission when graph wiring
// supplies its own shaping transform. Both predicates must hold.
func RequireAdmission(transform facade.Transform, prior facade.InboxAdmission) facade.Transform {
	if prior == nil {
		return transform
	}
	return inheritedAdmission{Transform: transform, prior: prior}
}

type inheritedAdmission struct {
	facade.Transform
	prior facade.InboxAdmission
}

func (p inheritedAdmission) Admission() facade.InboxAdmission { return p }
func (p inheritedAdmission) Admit(ctx context.Context, inbox map[string]any, edges map[string]facade.EdgeState) (bool, error) {
	admitted, err := p.prior.Admit(ctx, inbox, edges)
	if err != nil || !admitted {
		return admitted, err
	}
	if contextual, ok := p.Transform.(facade.InboxTransform); ok && contextual.Admission() != nil {
		return contextual.Admission().Admit(ctx, inbox, edges)
	}
	return true, nil
}
func (p inheritedAdmission) ApplyInbox(ctx context.Context, inbox map[string]any, edges map[string]facade.EdgeState) (facade.Record, error) {
	if contextual, ok := p.Transform.(facade.InboxTransform); ok {
		return contextual.ApplyInbox(ctx, inbox, edges)
	}
	return p.Transform.Apply(bind.NewDocRecord(inbox))
}

// Existing wire transforms also include native algorithms handled by compile;
// preserve their validation and diagnostics while AOT-checking script evaluators.
func validateSource(reg dsl.Registry, transform aot.Transform) error {
	evaluator, ok := reg.Get(transform.Type())
	if !ok {
		return nil
	}
	if validator, ok := evaluator.(dsl.Validator); ok {
		return validator.Validate(transform.Body())
	}
	return nil
}

func (p inboundProgram) Required() []string { return append([]string(nil), p.required...) }
func (p inboundProgram) Validate() error {
	names := map[string]bool{}
	for _, name := range p.required {
		if name == "" || names[name] {
			return fmt.Errorf("required transform inputs must be nonempty and unique")
		}
		names[name] = true
	}
	if p.typ != "" {
		if err := dsl.Validate(p.reg, p.typ, p.body); err != nil {
			return err
		}
	}
	if p.admission != nil {
		return dsl.Validate(p.reg, p.admission.Type, p.admission.Body)
	}
	return nil
}
func (p inheritedAdmission) Validate() error {
	if validator, ok := p.Transform.(interface{ Validate() error }); ok {
		return validator.Validate()
	}
	return nil
}

func validateExchangePrograms(ex aot.AOTExchange, reg dsl.Registry, o options) error {
	if transform := ex.Response().Transform(); transform != nil {
		if transform.Type() != "" {
			if err := validateSource(reg, transform); err != nil {
				return err
			}
		}
		if declared, ok := transform.(aot.AdmittedTransform); ok && declared.Admission() != nil {
			return fmt.Errorf("response admission is not an inbound dependency; declare it on the consumer transform")
		}
	}
	if request, ok := ex.Request().(aot.RequestTransform); ok {
		if transform := request.RequestTransform(); transform != nil && transform.Type() != "" {
			if err := validateSource(reg, transform); err != nil {
				return err
			}
		}
	}
	if o.overrideResponse && o.programType != "" {
		if evaluator, ok := reg.Get(o.programType); ok {
			if validator, ok := evaluator.(dsl.Validator); ok {
				return validator.Validate(o.programBody)
			}
		}
	}
	return nil
}

func requiredPresent(inbox map[string]any, names []string) bool {
	for _, name := range names {
		value, present := inbox[name]
		if !present || value == nil {
			return false
		}
		if text, ok := value.(string); ok && text == "" {
			return false
		}
	}
	return true
}
