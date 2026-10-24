package docx

import (
	"fmt"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/httpx"
	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
	"github.com/stackql-labs/omnisdk/pkg/docparse/dsl"
)

type requestBodyProgram struct {
	registry dsl.Registry
	program  aot.Transform
}

func (p requestBodyProgram) Apply(in facade.Page) (facade.Record, error) {
	body, err := p.registry.Eval(p.program.Type(), dsl.Context{}, p.program.Body(), httpx.ReqBody(in))
	if err != nil {
		return nil, err
	}
	return httpx.NewRequestRecord(httpx.Method(in), httpx.URL(in), httpx.Header(in), body), nil
}

type unsupportedProgramError struct{ language string }

func (e unsupportedProgramError) Error() string {
	return fmt.Sprintf("unsupported template type: %s", e.language)
}
func (e unsupportedProgramError) ProgramType() string { return e.language }

var _ facade.UnsupportedProgramError = unsupportedProgramError{}
