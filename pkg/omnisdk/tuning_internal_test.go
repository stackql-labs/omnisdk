package omnisdk

import (
	"context"
	"testing"

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
