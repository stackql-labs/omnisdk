package omnisdk

import (
	"context"
	"errors"
	"github.com/stackql-labs/omnisdk/internal/system_g/admit"
	"testing"
	"time"

	"github.com/stackql-labs/omnisdk/internal/system_g/httpx"
)

func TestProviderPageLimitInheritanceAndIsolation(t *testing.T) {
	policies := newProviderPolicies(Args{
		Tuning: Tuning{PageLimit: 20},
		TuningByProvider: map[string]ProviderTuning{
			"limited":                  {PageLimit: 2},
			"unlimited":                {PageLimit: -1},
			"inherited":                {},
			"stackql_unstable_limited": {PageLimit: 3},
		},
	})
	for provider, want := range map[string]int{"limited": 3, "unlimited": -1, "inherited": 20, "other": 20} {
		for range 2 {
			ctx := policies.ForProvider(context.Background(), provider)
			if got := httpx.PageLimit(ctx); got != want {
				t.Errorf("%s: limit=%d want=%d", provider, got, want)
			}
		}
	}
}

func TestTenancyTotalAdmissionSpansHosts(t *testing.T) {
	args := Args{ProviderAliases: map[string]ProviderBinding{"one": {Provider: "demo", Tenancy: "one", Tuning: ProviderTuning{MaxConcurrent: 1}}, "two": {Provider: "demo", Tenancy: "two", Tuning: ProviderTuning{MaxConcurrent: 1}}}}
	policies := newProviderPolicies(args)
	one := policies.ForProvider(context.Background(), scopeName("demo", "one"))
	two := policies.ForProvider(context.Background(), scopeName("demo", "two"))
	first, err := admit.From(one).For("host-a").Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	independent, err := admit.From(two).For("host-a").Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	independent.Release()
	deadline, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if token, err := admit.From(one).For("host-b").Acquire(deadline); !errors.Is(err, context.DeadlineExceeded) {
		if token != nil {
			token.Release()
		}
		first.Release()
		t.Fatalf("another host bypassed account cap: %v", err)
	}
	first.Release()
	deadline2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	next, err := admit.From(one).For("host-b").Acquire(deadline2)
	if err != nil {
		t.Fatal(err)
	}
	next.Release()
}
