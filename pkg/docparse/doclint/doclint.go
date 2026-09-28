// Package doclint finds incongruities in provider documents: places where a document does not say
// what a query needs it to — rows it does not locate, references that go nowhere, methods it leaves
// indistinguishable. Nothing here fixes a document or blocks a query; findings are reported so the
// documents can be corrected in their own time, and a caller can patch its own view meanwhile.
//
// Analyzers are grouped in families, one per document format; For returns a format's family.
package doclint

import (
	"fmt"
	"sort"
	"strings"

	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
)

// Severity ranks a finding.
type Severity string

const (
	// Error: part of the document cannot be used at all.
	Error Severity = "error"
	// Warning: the document can be used, but a query will probably get something other than meant.
	Warning Severity = "warning"
)

// Finding is one incongruity.
type Finding struct {
	Rule     string   `json:"rule"`
	Severity Severity `json:"severity"`
	// Address is the provider, service or resource concerned, as addresses name them.
	Address string `json:"address"`
	// Method is the method concerned, where the finding is about one.
	Method  string `json:"method,omitempty"`
	Message string `json:"message"`
}

// Analyzer is one rule.
type Analyzer interface {
	Name() string
	// Describe says what the rule looks for and why it matters.
	Describe() string
}

// CatalogAnalyzer checks a provider as a whole.
type CatalogAnalyzer interface {
	Analyzer
	Catalog(provider string, c aot.Catalog) []Finding
}

// ExchangeAnalyzer checks the exchanges one resource binds to one verb, in document order.
type ExchangeAnalyzer interface {
	Analyzer
	Exchanges(address, verb string, exs []aot.AOTExchange) []Finding
}

var families = map[string][]Analyzer{}

// Register adds a format's family. A format registered twice is a bug in the registering package.
func Register(format string, family ...Analyzer) {
	if _, dup := families[format]; dup {
		panic(fmt.Sprintf("doclint: format %q registered twice", format))
	}
	families[format] = family
}

// For returns a document format's analyzers.
func For(format string) ([]Analyzer, bool) {
	a, ok := families[format]
	return a, ok
}

// Formats lists the formats with a family, sorted.
func Formats() []string {
	out := make([]string, 0, len(families))
	for f := range families {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// Verbs are the SQL verbs a resource's exchanges are checked under.
var Verbs = []string{"select", "insert", "update", "delete"}

// Run applies analyzers to the providers named, or to every provider when none is. A provider that
// cannot be opened, a service that does not parse, and a verb that does not resolve are findings
// themselves: resolving is the first thing a query does.
func Run(reg aot.Registry, analyzers []Analyzer, providers ...string) []Finding {
	if len(providers) == 0 {
		providers = reg.Providers()
	}
	var out []Finding
	for _, p := range providers {
		c, err := reg.Catalog(p)
		if err != nil {
			out = append(out, Finding{Rule: "resolves", Severity: Error, Address: p, Message: err.Error()})
			continue
		}
		for _, a := range analyzers {
			if ca, ok := a.(CatalogAnalyzer); ok {
				out = append(out, ca.Catalog(p, c)...)
			}
		}
		for _, svc := range c.Services() {
			resources, err := c.Resources(svc)
			if err != nil {
				out = append(out, Finding{Rule: "resolves", Severity: Error, Address: p + "." + svc, Message: err.Error()})
				continue
			}
			for _, res := range resources {
				addr := p + "." + svc + "." + res
				declared := declaredVerbs(c, svc, res)
				for _, verb := range Verbs {
					if !declared[verb] {
						continue
					}
					exs, err := c.Operations(addr, verb)
					if err != nil {
						out = append(out, Finding{Rule: "resolves", Severity: Error, Address: addr,
							Message: fmt.Sprintf("%s: %v", verb, err)})
						continue
					}
					for _, a := range analyzers {
						if ea, ok := a.(ExchangeAnalyzer); ok {
							out = append(out, ea.Exchanges(addr, verb, exs)...)
						}
					}
				}
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Address != out[j].Address {
			return out[i].Address < out[j].Address
		}
		return out[i].Rule < out[j].Rule
	})
	return out
}

// declaredVerbs are the verbs a resource binds at least one method to.
func declaredVerbs(c aot.Catalog, svc, res string) map[string]bool {
	out := map[string]bool{}
	ms, err := c.Methods(svc, res)
	if err != nil {
		return out
	}
	for _, m := range ms {
		if v := strings.ToLower(m.SQLVerb()); v != "" {
			out[v] = true
		}
	}
	return out
}
