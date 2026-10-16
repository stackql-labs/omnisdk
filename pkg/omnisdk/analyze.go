package omnisdk

import (
	"context"
	"fmt"

	"github.com/stackql-labs/omnisdk/internal/analyze"
	"github.com/stackql-labs/omnisdk/internal/docsem"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
)

// analyzerFor binds statements against one provider's documents. Every entry point that turns a verb
// on a resource into a method comes through here, so a query, an ad hoc write and an IaC run choose
// the same method for the same request.
func analyzerFor(cat aot.Catalog) facade.Analyzer {
	return analyze.New(docsem.Capabilities(cat), analyze.PermitAll())
}

// chooseExchange binds verb on addr to the operation the analyzer chooses. available are the inputs
// that will have a value — an empty one does not count — and sent are those the statement sets,
// which the operation must accept.
//
// Planning happens when a plan is built, before any context exists, so the analysis runs under none.
func chooseExchange(c aot.Catalog, addr, verb string, available map[string]any, sent []string) (aot.AOTExchange, error) {
	names := make([]string, 0, len(available))
	for k, v := range available {
		if v != "" {
			names = append(names, k)
		}
	}
	m, err := analyzerFor(c).Analyze(context.Background(), analyze.NewStatement(facade.AnonymousPrincipal, addr, verb, names, sent, ""))
	if err != nil {
		return nil, err
	}
	ex, ok := docsem.Exchange(m)
	if !ok {
		return nil, fmt.Errorf("method %s is not a document operation", m.Name())
	}
	return ex, nil
}
