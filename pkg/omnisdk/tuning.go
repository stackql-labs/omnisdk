package omnisdk

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/stackql-labs/omnisdk/internal/system_g/admit"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/httpx"
	"github.com/stackql-labs/omnisdk/internal/system_g/plan"
	"github.com/stackql-labs/omnisdk/internal/system_g/requestpolicy"
	"github.com/stackql-labs/omnisdk/internal/system_g/retry"
	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
)

// DefaultRequestTimeout is the fallback deadline for a request and its response.
const DefaultRequestTimeout = httpx.DefaultRequestTimeout

// ProviderTuning overrides request settings for one provider.
// Zero fields inherit the global Tuning value, then library defaults. Whole-run
// budgets (Timeout, Limit, Parallelism, buffering) stay global.
type ProviderTuning struct {
	// PageLimit inherits when zero; negative explicitly removes a global cap.
	PageLimit int
	// RequestTimeout uses a negative duration to explicitly disable its deadline.
	RequestTimeout time.Duration
	// RetryTries, RetryRate and MaxPerHost configure HTTP retries and admission,
	// with the same meaning as their global Tuning counterparts.
	RetryTries int
	RetryRate  float64
	MaxPerHost int
}

type providerPlan struct {
	plan.Plan
	provider string
}

func (p providerPlan) Exchanges() []plan.ExchangeSpec {
	specs := p.Plan.Exchanges()
	out := make([]plan.ExchangeSpec, len(specs))
	for i, spec := range specs {
		out[i] = providerExchange{ExchangeSpec: spec, provider: p.provider}
	}
	return out
}

type providerExchange struct {
	plan.ExchangeSpec
	provider string
}

func (s providerExchange) Make(bound map[string]any) facade.Operator {
	return providerOperator{Operator: s.ExchangeSpec.Make(bound), provider: s.provider}
}

type providerOperator struct {
	facade.Operator
	provider string
}

func (o providerOperator) Open(ctx context.Context) facade.Records {
	return o.Operator.Open(requestpolicy.ForProvider(ctx, o.provider))
}

type providerPolicy struct {
	pageLimit  int
	timeout    time.Duration
	retry      facade.RetryPolicy
	admissions facade.Admissions
}

type providerPolicies struct {
	args     Args
	mu       sync.Mutex
	policies map[string]providerPolicy
}

func newProviderPolicies(args Args) facade.RequestPolicy {
	return &providerPolicies{args: args, policies: map[string]providerPolicy{}}
}

func (p *providerPolicies) ForProvider(ctx context.Context, name string) context.Context {
	name = strings.TrimPrefix(name, aot.DefaultProviderPrefix)
	p.mu.Lock()
	policy, ok := p.policies[name]
	if !ok {
		t := p.args.Tuning
		o, found := p.args.TuningByProvider[aot.DefaultProviderPrefix+name]
		if !found {
			o = p.args.TuningByProvider[name]
		}
		if o.RequestTimeout != 0 {
			t.RequestTimeout = o.RequestTimeout
		}
		if o.PageLimit != 0 {
			t.PageLimit = o.PageLimit
		}
		if o.RetryTries != 0 {
			t.RetryTries = o.RetryTries
		}
		if o.RetryRate != 0 {
			t.RetryRate = o.RetryRate
		}
		if o.MaxPerHost != 0 {
			t.MaxPerHost = o.MaxPerHost
		}
		policy = providerPolicy{timeout: t.RequestTimeout, pageLimit: t.PageLimit,
			retry: retry.New(retry.Config{
				Tries:    orInt(t.RetryTries, 4),
				Backoff:  retry.NewDecorrelatedJitter(250*time.Millisecond, 30*time.Second),
				Governor: retry.NewRateGovernor(orFloat(t.RetryRate, 20), 5),
			}),
			admissions: admit.PerScope(orInt(t.MaxPerHost, 8)),
		}
		p.policies[name] = policy
	}
	p.mu.Unlock()
	ctx = httpx.WithRequestTimeout(ctx, policy.timeout)
	ctx = httpx.WithPageLimit(ctx, policy.pageLimit)
	ctx = retry.WithPolicy(ctx, policy.retry)
	return admit.WithAdmissions(ctx, policy.admissions)
}
