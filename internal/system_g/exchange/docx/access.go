package docx

import (
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/plan"
	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
)

func protectSpec(spec plan.ExchangeSpec, exchange aot.AOTExchange, provider string) plan.ExchangeSpec {
	return plan.WithAccess(spec, true, facade.DocumentPermissions(provider, exchange.Name(), exchange.OperationID())...)
}
