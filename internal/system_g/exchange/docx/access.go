package docx

import (
	"context"

	"github.com/stackql-labs/omnisdk/internal/system_g/buffer"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/plan"
	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
)

type accessSpec struct {
	plan.ExchangeSpec
	resource, provider string
}

func protectSpec(s plan.ExchangeSpec, ex aot.AOTExchange, provider string) plan.ExchangeSpec {
	if provider == "" {
		provider = "unscoped"
	}
	return accessSpec{s, "operation/" + provider + "/" + ex.Name() + "/" + ex.OperationID(), provider}
}
func (s accessSpec) Make(bound map[string]any) facade.Operator {
	return accessOp{s.ExchangeSpec.Make(bound), s.resource, s.provider}
}

type accessOp struct {
	op                 facade.Operator
	resource, provider string
}

func (o accessOp) Open(ctx context.Context) facade.Records {
	for _, permission := range [][2]string{{o.resource, "execute"}, {"credential/" + o.provider, "use"}} {
		if permission[0] == "credential/"+o.provider && permission[1] == "execute" {
			continue
		}
		if err := facade.CheckAccess(ctx, permission[0], permission[1]); err != nil {
			b := buffer.NewBuffer(1, 1, 0)
			b.Complete(err)
			return b.Reader()
		}
	}
	return o.op.Open(facade.WithAuthorizedExchange(ctx))
}
