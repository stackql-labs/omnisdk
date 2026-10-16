// Package analyze binds a statement to the method that carries it out.
//
// One rule serves every caller — a query's SELECT, an ad hoc UPDATE, an IaC create, the update that
// converges drift — so a resource is acted on the same way whichever entry point asked. The rule:
//
//   - a method runs only if every input it requires is available;
//   - it is chosen only if it accepts every input the statement sends, since an input it does not
//     declare is dropped or rejected, and either applies something other than what was asked;
//   - the most specific survivor wins, measured by required inputs met, because supplying an
//     identifier means asking about that one thing;
//   - a tie is ambiguous and is reported, never broken by guessing.
//
// The documents say what is possible (facade.Capabilities); a policy narrows it (facade.Policy).
package analyze

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

type analyzer struct {
	caps   facade.Capabilities
	policy facade.Policy
}

// New returns an Analyzer over caps, with every choice subject to policy.
func New(caps facade.Capabilities, policy facade.Policy) facade.Analyzer {
	return analyzer{caps: caps, policy: policy}
}

func (a analyzer) Analyze(ctx context.Context, s facade.Statement) (facade.Method, error) {
	methods, err := a.caps.Methods(s.Resource(), s.Verb())
	if err != nil {
		return nil, err
	}
	available := map[string]bool{}
	for _, in := range s.Available() {
		available[in] = true
	}
	if name := s.Pinned(); name != "" {
		return a.pinned(ctx, s, methods, available, name)
	}

	var best []facade.Method
	bestScore := -1
	var reasons []string
	for _, m := range methods {
		if why := fits(m, s, available); why != "" {
			reasons = append(reasons, m.Name()+" "+why)
			continue
		}
		if err := a.policy.Permit(ctx, s, m); err != nil {
			reasons = append(reasons, m.Name()+" is not permitted: "+err.Error())
			continue
		}
		switch score := len(m.Required()); {
		case score > bestScore:
			best, bestScore = []facade.Method{m}, score
		case score == bestScore:
			best = append(best, m)
		}
	}
	if len(best) > 1 && s.Verb() == "select" {
		// A read that needs a request body — Azure's check_name_availability, bound to select beside
		// list — needs content the query does not have. Where methods tie, the ones that do not are
		// the reads.
		if reads := slices.DeleteFunc(slices.Clone(best), facade.Method.TakesBody); len(reads) > 0 {
			best = reads
		}
	}
	switch len(best) {
	case 0:
		return nil, fmt.Errorf("no satisfiable %s method (%s)", s.Verb(), strings.Join(reasons, "; "))
	case 1:
		return best[0], nil
	}
	names := make([]string, len(best))
	for i, m := range best {
		names[i] = m.Name()
	}
	return nil, fmt.Errorf("several %s methods are satisfiable (%s); supply a parameter that distinguishes them",
		s.Verb(), strings.Join(names, ", "))
}

// pinned honours a method the caller named. Naming it decides which method, not whether it can run:
// it must still exist, have its inputs, accept what is sent, and be permitted.
func (a analyzer) pinned(ctx context.Context, s facade.Statement, methods []facade.Method, available map[string]bool, name string) (facade.Method, error) {
	i := slices.IndexFunc(methods, func(m facade.Method) bool { return m.Name() == name })
	if i < 0 {
		return nil, fmt.Errorf("%s declares no %s method %q", s.Resource(), s.Verb(), name)
	}
	m := methods[i]
	if why := fits(m, s, available); why != "" {
		return nil, fmt.Errorf("%s %s", name, why)
	}
	if err := a.policy.Permit(ctx, s, m); err != nil {
		return nil, fmt.Errorf("%s is not permitted: %w", name, err)
	}
	return m, nil
}

// fits says why m cannot carry out s, or "" where it can.
func fits(m facade.Method, s facade.Statement, available map[string]bool) string {
	for _, r := range m.Required() {
		if !available[r] {
			return "needs " + r
		}
	}
	for _, in := range s.Sent() {
		if !m.Accepts(in) {
			return "does not accept " + in
		}
	}
	return ""
}

type statement struct {
	resource, verb  string
	available, sent []string
	pinned          string
}

// NewStatement builds a Statement. pinned may be empty, meaning the analyzer chooses.
func NewStatement(resource, verb string, available, sent []string, pinned string) facade.Statement {
	return statement{resource: resource, verb: verb, available: available, sent: sent, pinned: pinned}
}

func (s statement) Resource() string    { return s.resource }
func (s statement) Verb() string        { return s.verb }
func (s statement) Available() []string { return s.available }
func (s statement) Sent() []string      { return s.sent }
func (s statement) Pinned() string      { return s.pinned }

type permitAll struct{}

// PermitAll is the policy that refuses nothing: every method the documents allow may be chosen.
func PermitAll() facade.Policy { return permitAll{} }

func (permitAll) Permit(context.Context, facade.Statement, facade.Method) error { return nil }
