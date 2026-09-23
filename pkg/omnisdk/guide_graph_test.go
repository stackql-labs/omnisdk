package omnisdk_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
)

// serviceAccountKey mints a syntactically valid key so credential resolution succeeds and the test
// exercises PLAN COMPOSITION rather than stopping at authentication.
func mustPKCS8(t *testing.T, key *rsa.PrivateKey) []byte {
	t.Helper()
	b, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("pkcs8: %v", err)
	}
	return b
}

func serviceAccountKey(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: mustPKCS8(t, key)})
	b, err := json.Marshal(map[string]string{
		"type":           "service_account",
		"client_email":   "test@example.iam.gserviceaccount.com",
		"private_key_id": "test",
		"private_key":    string(pemBytes),
		"token_uri":      "https://oauth2.googleapis.com/token",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// The graphs published in the developer guide must COMPOSE. Every failure handed to a reader so far
// has been of one kind — an exchange whose required input nothing supplies, two documents whose
// methods share a name, an auth exchange never wired in — and all of them surface here, before any
// request is made. A documented command that cannot build is worse than no command.
func TestGuideGraphsCompose(t *testing.T) {
	requireCorpus(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIATEST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("GOOGLE_CREDENTIALS", serviceAccountKey(t))
	t.Setenv("AZURE_TENANT_ID", "tenant")
	t.Setenv("AZURE_CLIENT_ID", "client")
	t.Setenv("AZURE_CLIENT_SECRET", "secret")

	cases := []struct {
		name      string
		addresses []string
		wirings   []omnisdk.Wiring
		overrides []omnisdk.Override
		params    map[string]string
	}{
		{
			name: "aws vpcs and subnets",
			addresses: []string{
				"stackql_unstable_aws.ec2.vpcs",
				"stackql_unstable_aws.ec2.subnets",
			},
			wirings: []omnisdk.Wiring{omnisdk.NewWiring(
				"stackql_unstable_aws.ec2.subnets",
				[]omnisdk.Inbound{omnisdk.NewInbound("stackql_unstable_aws.ec2.vpcs", "VpcId", "vpc_id")},
				"golang_template_json_v0.1.0",
				`{"Filter.1.Name":"vpc-id","Filter.1.Value.1":"{{ .vpc_id }}"}`,
				"Filter.1.Name", "Filter.1.Value.1",
			)},
			params: map[string]string{"region": "us-east-1"},
		},
		{
			name: "google networks and subnetworks",
			addresses: []string{
				"stackql_unstable_google.compute.networks",
				"stackql_unstable_google.compute.subnetworks",
			},
			overrides: []omnisdk.Override{
				omnisdk.NewOverride("stackql_unstable_google.compute.networks", "$.items", "", "", ""),
				omnisdk.NewOverride("stackql_unstable_google.compute.subnetworks", "$.items", "", "", ""),
			},
			wirings: []omnisdk.Wiring{omnisdk.NewWiring(
				"stackql_unstable_google.compute.subnetworks",
				[]omnisdk.Inbound{omnisdk.NewInbound("stackql_unstable_google.compute.networks", "selfLink", "network")},
				"golang_template_json_v0.1.0",
				`{"filter":"network=\"{{ .network }}\""}`,
				"filter",
			)},
			params: map[string]string{"project": "PROJECT", "region": "us-central1"},
		},
		{
			name: "azure virtual networks and subnets",
			addresses: []string{
				"stackql_unstable_azure.network.virtual_networks",
				"stackql_unstable_azure.network.subnets",
			},
			wirings: []omnisdk.Wiring{omnisdk.NewWiring(
				"stackql_unstable_azure.network.subnets",
				[]omnisdk.Inbound{
					omnisdk.NewInbound("stackql_unstable_azure.network.virtual_networks", "name", "vnet"),
					omnisdk.NewInbound("stackql_unstable_azure.network.virtual_networks", "id", "arm_id"),
				},
				"golang_template_json_v0.1.0",
				`{"virtual_network_name":"{{ .vnet }}","resource_group_name":"{{ index (split "/" .arm_id) 4 }}"}`,
				"virtual_network_name", "resource_group_name",
			)},
			params: map[string]string{"subscription_id": "sub"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, err := omnisdk.NewGraph(tc.addresses, tc.wirings, tc.overrides...)
			if err != nil {
				t.Fatalf("graph: %v", err)
			}
			// An unroutable endpoint: the plan must build, and the run must then fail at the wire
			// rather than before it.
			pl, err := omnisdk.NewGraphSelectQuery(corpus, g, omnisdk.Args{
				Endpoint: "http://127.0.0.1:9",
				Params:   tc.params,
			})
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			rows, err := pl.Open(context.Background())
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer rows.Close()
			for rows.Next() {
			}
			err = rows.Err()
			if err == nil {
				return // nothing to reach, but nothing refused to build either
			}
			// These are the composition failures. Anything else is the network, which is expected.
			for _, refusal := range []string{
				"unsatisfied required inputs",
				"dependency cycle",
				"no credentials were supplied",
				"requires input",
				"requires parameter",
			} {
				if strings.Contains(err.Error(), refusal) {
					t.Errorf("the plan did not compose: %v", err)
				}
			}
		})
	}
}
