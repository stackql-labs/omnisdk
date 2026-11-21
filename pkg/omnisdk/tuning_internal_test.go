package omnisdk

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/stackql-labs/omnisdk/internal/system_g/admit"
	"net/http"
	"net/url"
	"strings"
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

func TestProviderTuningJSONBinding(t *testing.T) {
	const input = `{"provider":"aws","tenancy":"stackql-account","tuning":{"max_concurrent":4,"max_per_host":4,"page_limit":2,"request_timeout":1000000000,"retry_tries":3,"retry_rate":2.5}}`
	var binding ProviderBinding
	decoder := json.NewDecoder(strings.NewReader(input))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&binding); err != nil {
		t.Fatal(err)
	}
	want := ProviderTuning{MaxConcurrent: 4, MaxPerHost: 4, PageLimit: 2, RequestTimeout: time.Second, RetryTries: 3, RetryRate: 2.5}
	if binding.Tuning != want {
		t.Fatalf("got %+v, want %+v", binding.Tuning, want)
	}
	encoded, err := json.Marshal(binding)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"max_concurrent", "max_per_host", "page_limit", "request_timeout", "retry_tries", "retry_rate"} {
		if !strings.Contains(string(encoded), `"`+key+`":`) {
			t.Errorf("missing JSON field %s in %s", key, encoded)
		}
	}
}

func TestProviderProxyPortInheritanceAndIsolation(t *testing.T) {
	original, err := url.Parse("http://user:pass@[::1]:8030")
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{Proxy: func(req *http.Request) (*url.URL, error) {
		if req.URL.Hostname() == "direct.example" {
			return nil, nil
		}
		return original, nil
	}}
	client := &http.Client{Transport: transport, Timeout: time.Second}
	policies := newProviderPolicies(Args{
		TuningByProvider: map[string]ProviderTuning{"widgets": {ProxyPort: 8031}},
		ProviderAliases: map[string]ProviderBinding{
			"override": {Provider: "widgets", Tenancy: "override", Tuning: ProviderTuning{ProxyPort: 8032}},
			"original": {Provider: "widgets", Tenancy: "original", Tuning: ProviderTuning{ProxyPort: -1}},
		},
	})
	for name, want := range map[string]string{
		"widgets": "8031", scopeName("widgets", "inherit"): "8031",
		scopeName("widgets", "override"): "8032", scopeName("widgets", "original"): "8030", "other": "8030",
	} {
		ctx := policies.ForProvider(httpx.WithClient(context.Background(), client), name)
		resolved := httpx.ClientFrom(ctx, nil)
		if resolved.Timeout != client.Timeout {
			t.Fatalf("%s: proxy override dropped client timeout", name)
		}
		proxy := resolved.Transport.(*http.Transport).Proxy
		got, err := proxy(&http.Request{URL: &url.URL{Host: "proxy.example"}})
		if err != nil || got.Port() != want || got.Hostname() != "::1" || got.User != original.User {
			t.Fatalf("%s: proxy=%v, want port %s with the original hostname and credentials, err=%v", name, got, want, err)
		}
		got, err = proxy(&http.Request{URL: &url.URL{Host: "direct.example"}})
		if err != nil || got != nil {
			t.Fatalf("%s: direct request produced proxy=%v, err=%v", name, got, err)
		}
	}
	if original.Port() != "8030" {
		t.Fatal("proxy override mutated the proxy URL")
	}
}
