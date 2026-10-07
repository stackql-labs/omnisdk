package doclint

import (
	"fmt"
	"sort"
	"strings"

	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
)

// Format is the stackql provider-document format.
const Format = "stackql"

// DefaultObjectKey is where a list's rows are when a stackql document declares no objectKey.
const DefaultObjectKey = "items"

func init() {
	Register(Format, missingDocuments{}, rowsNotLocated{}, noRowSchema{}, indistinguishableSelects{})
}

// missingDocuments: a provider names a service document its bundle lacks.
type missingDocuments struct{}

func (missingDocuments) Name() string { return "missing_document" }
func (missingDocuments) Describe() string {
	return "The provider lists a service whose document is not in the bundle; the service cannot be addressed."
}

func (m missingDocuments) Catalog(provider string, c aot.Catalog) []Finding {
	mc, ok := c.(interface{ MissingDocuments() map[string]string })
	if !ok {
		return nil
	}
	missing := mc.MissingDocuments()
	names := make([]string, 0, len(missing))
	for n := range missing {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []Finding
	for _, n := range names {
		out = append(out, Finding{Rule: m.Name(), Severity: Error, Address: provider + "." + n,
			Message: fmt.Sprintf("names document %s, which is not in the bundle", missing[n])})
	}
	return out
}

// rowsNotLocated: a select declares no objectKey, its response has no list at the default $.items,
// but does have a list elsewhere — the rows are probably there, and the document should say so.
type rowsNotLocated struct{}

func (rowsNotLocated) Name() string { return "rows_not_located" }
func (rowsNotLocated) Describe() string {
	return "A select declares no objectKey and its response has no list at the default $.items, but has one " +
		"elsewhere; without an objectKey the whole envelope is one row."
}

func (r rowsNotLocated) Exchanges(address, verb string, exs []aot.AOTExchange) []Finding {
	if verb != "select" {
		return nil
	}
	var out []Finding
	for _, ex := range exs {
		resp := ex.Response()
		sch := resp.Schema()
		if strings.TrimSpace(resp.ObjectKey()) != "" || sch == nil {
			continue
		}
		if _, isList := sch.Items(); isList {
			continue // the body is the list
		}
		if p, ok := sch.Property(DefaultObjectKey); ok {
			if _, isList := p.Items(); isList {
				continue
			}
		}
		var lists []string
		for _, name := range sch.Properties() {
			if p, ok := sch.Property(name); ok {
				if _, isList := p.Items(); isList {
					lists = append(lists, "$."+name)
				}
			}
		}
		if len(lists) == 0 {
			continue // a single object: the body is its one row
		}
		out = append(out, Finding{Rule: r.Name(), Severity: Warning, Address: address, Method: ex.Name(),
			Message: fmt.Sprintf("no objectKey and no list at $.%s; rows are probably at %s", DefaultObjectKey, strings.Join(lists, " or "))})
	}
	return out
}

// noRowSchema: a select declares no response schema, so its columns are unknown.
type noRowSchema struct{}

func (noRowSchema) Name() string { return "no_row_schema" }
func (noRowSchema) Describe() string {
	return "A select declares no response schema: its columns are unknown, so SELECT * and unqualified columns cannot resolve."
}

func (n noRowSchema) Exchanges(address, verb string, exs []aot.AOTExchange) []Finding {
	if verb != "select" {
		return nil
	}
	var out []Finding
	for _, ex := range exs {
		if ex.Response().Schema() == nil {
			out = append(out, Finding{Rule: n.Name(), Severity: Warning, Address: address, Method: ex.Name(),
				Message: "declares no response schema"})
		}
	}
	return out
}

// indistinguishableSelects: two select methods need exactly the same parameters and both do or both
// do not take a body — no query can choose between them.
type indistinguishableSelects struct{}

func (indistinguishableSelects) Name() string { return "indistinguishable_selects" }
func (indistinguishableSelects) Describe() string {
	return "Two select methods need exactly the same parameters and are alike in taking a body; a query " +
		"supplying those parameters cannot choose between them."
}

func (d indistinguishableSelects) Exchanges(address, verb string, exs []aot.AOTExchange) []Finding {
	if verb != "select" {
		return nil
	}
	groups := map[string][]string{}
	var keys []string
	for _, ex := range exs {
		var req []string
		for _, p := range ex.Request().Parameters() {
			if p.Required() {
				req = append(req, p.Name())
			}
		}
		sort.Strings(req)
		key := strings.Join(req, ",") + fmt.Sprintf("|body=%t", ex.Request().BodyMediaType() != "")
		if _, seen := groups[key]; !seen {
			keys = append(keys, key)
		}
		groups[key] = append(groups[key], ex.Name())
	}
	var out []Finding
	for _, k := range keys {
		if names := groups[k]; len(names) > 1 {
			params, _, _ := strings.Cut(k, "|")
			out = append(out, Finding{Rule: d.Name(), Severity: Warning, Address: address, Method: strings.Join(names, ", "),
				Message: fmt.Sprintf("%s all need exactly (%s)", strings.Join(names, ", "), params)})
		}
	}
	return out
}
