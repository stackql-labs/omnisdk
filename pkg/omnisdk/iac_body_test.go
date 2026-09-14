package omnisdk_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
)

// A create against a JSON API sends the intent as the request BODY. Scattering its fields into the
// query is how a document meant for one API goes out shaped for another — and the provider reads it
// as a call with no content at all.
func TestGoogleCreateSendsTheIntentAsABody(t *testing.T) {
	t.Setenv("GOOGLE_CREDENTIALS", serviceAccountKey(t))

	var body, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/token") {
			fmt.Fprint(w, `{"access_token":"tok","expires_in":3600}`)
			return
		}
		b, _ := io.ReadAll(r.Body)
		body, path = string(b), r.URL.Path
		fmt.Fprint(w, `{"name":"op-1","targetId":"net-1"}`)
	}))
	defer srv.Close()

	res := []omnisdk.ManagedResource{
		omnisdk.NewResource("google/compute/network", "google", "compute.networks",
			[]byte(`{"name":"demo-net","autoCreateSubnetworks":false}`),
			map[string]string{"project": "demo"}, nil, "", "",
			// Client-named: the identity is what the caller asked for, so nothing has to be read back
			// out of an async Operation to know what was made.
			"", "network", ""),
	}
	pl, err := omnisdk.Converge(corpus, "bodydemo", t.TempDir(), "run-1", res, omnisdk.Args{
		Endpoint: srv.URL,
		Params:   map[string]string{"project": "demo"},
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	rows, err := pl.Open(context.Background())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for rows.Next() {
	}
	rows.Close()

	if body == "" {
		t.Fatalf("no request body was sent; path was %q", path)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("body %q is not JSON: %v", body, err)
	}
	if got["name"] != "demo-net" {
		t.Errorf("body = %s, want the intent document sent whole", body)
	}
	if strings.Contains(path, "autoCreateSubnetworks") {
		t.Errorf("path = %q, want intent fields in the body, not the query", path)
	}
}

// A second run against unchanged intent must issue no create. The read answers about one object, so
// what it reports as ACTUAL has to hold that object's fields where the desired document holds them —
// wrapped in the row list every projected reply shares, convergence compares a field against nothing
// and reports drift on a resource that never changed.
func TestRerunConvergesRatherThanReportingDrift(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIATEST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")

	var creates int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/xml")
		switch r.URL.Query().Get("Action") {
		case "CreateVpc":
			creates++
			fmt.Fprint(w, `<CreateVpcResponse><vpc><vpcId>vpc-1</vpcId><cidrBlock>10.0.0.0/16</cidrBlock></vpc></CreateVpcResponse>`)
		default:
			fmt.Fprint(w, `<DescribeVpcsResponse><vpcSet><item><vpcId>vpc-1</vpcId><cidrBlock>10.0.0.0/16</cidrBlock></item></vpcSet></DescribeVpcsResponse>`)
		}
	}))
	defer srv.Close()

	res := []omnisdk.ManagedResource{
		omnisdk.NewResource("aws/ec2/vpc", "aws", "ec2.vpcs",
			[]byte(`{"CidrBlock":"10.0.0.0/16"}`), nil, nil, "", "",
			"line_items.VpcId", "VpcId", "TagSpecification.1.Tag.1.Value"),
	}
	state := t.TempDir()
	run := func(id string) []omnisdk.Row {
		t.Helper()
		pl, err := omnisdk.Converge(corpus, "converge", state, id, res, omnisdk.Args{
			Endpoint: srv.URL, Params: map[string]string{"region": "us-east-1"},
		})
		if err != nil {
			t.Fatalf("plan: %v", err)
		}
		rows, err := pl.Open(context.Background())
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer rows.Close()
		var out []omnisdk.Row
		for rows.Next() {
			out = append(out, rows.Row())
		}
		return out
	}

	if got := run("run-1"); len(got) != 1 || got[0]["status"] != "applied" {
		t.Fatalf("first run = %v, want applied", got)
	}
	second := run("run-2")
	if len(second) != 1 || second[0]["status"] != "applied" {
		t.Fatalf("second run = %v, want it to converge", second)
	}
	if creates != 1 {
		t.Errorf("CreateVpc issued %d times, want 1 — the re-run should converge, not re-create or refuse", creates)
	}
}
