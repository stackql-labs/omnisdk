package omnisdk_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
)

// The Azure wiring must actually RUN its program, not merely compose: a template naming a function
// the evaluator does not have fails only when a row arrives, which is after every composition check
// has passed.
func TestAzureWiringProgramRuns(t *testing.T) {
	requireCorpus(t)
	t.Setenv("AZURE_TENANT_ID", "tenant")
	t.Setenv("AZURE_CLIENT_ID", "client")
	t.Setenv("AZURE_CLIENT_SECRET", "secret")

	var subnetURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/oauth2/"):
			fmt.Fprint(w, `{"access_token":"tok","expires_in":3600}`)
		case strings.Contains(r.URL.Path, "/subnets"):
			subnetURL = r.URL.Path
			fmt.Fprint(w, `{"value":[{"name":"subnet-1"}]}`)
		default:
			fmt.Fprint(w, `{"value":[{"name":"vnet-1","id":"/subscriptions/sub/resourceGroups/rg-1/providers/Microsoft.Network/virtualNetworks/vnet-1"}]}`)
		}
	}))
	defer srv.Close()

	const vnets = "stackql_unstable_azure.network.virtual_networks"
	const subnets = "stackql_unstable_azure.network.subnets"
	g, err := omnisdk.NewGraph([]string{vnets, subnets},
		[]omnisdk.Wiring{omnisdk.NewWiring(subnets,
			[]omnisdk.Inbound{
				omnisdk.NewInbound(vnets, "name", "vnet"),
				omnisdk.NewInbound(vnets, "id", "arm_id"),
			},
			"golang_template_json_v0.1.0",
			`{"virtual_network_name":"{{ .vnet }}","resource_group_name":"{{ index (split "/" .arm_id) 4 }}"}`,
			"virtual_network_name", "resource_group_name",
		)})
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	pl, err := omnisdk.NewGraphQuery(corpus, g, omnisdk.Args{
		Endpoint: srv.URL,
		Params:   map[string]string{"subscription_id": "sub"},
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
	if err := rows.Err(); err != nil {
		t.Fatalf("run: %v", err)
	}
	// The resource group lives only inside the producer's ARM id; reaching it is the whole point of
	// the inbound program.
	if !strings.Contains(subnetURL, "/resourceGroups/rg-1/") {
		t.Errorf("subnet request path = %q, want the resource group pulled out of the ARM id", subnetURL)
	}
	if !strings.Contains(subnetURL, "/virtualNetworks/vnet-1/") {
		t.Errorf("subnet request path = %q, want the vnet name bound", subnetURL)
	}
}
