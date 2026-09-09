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

// fakeEC2 answers the Query API well enough to provision: signed form POSTs in, XML out, with the
// tags a create stamps kept so they can be asserted.
// describe answers a read either by id or by the correlation tag filter, which is what lets an
// object be adopted when the ledger no longer knows about it.
func describe(w http.ResponseWriter, r *http.Request, act string, live, cidrs map[string]string) {
	set, item, idAttr := "vpcSet", "vpcId", "VpcId.1"
	if act == "DescribeSubnets" {
		set, item, idAttr = "subnetSet", "subnetId", "SubnetId.1"
	}
	id := r.PostForm.Get(idAttr)
	if want := r.PostForm.Get("Filter.1.Value.1"); want != "" {
		id = ""
		for candidate, corr := range live {
			if corr == want && strings.HasPrefix(candidate, strings.TrimSuffix(item, "Id")) {
				id = candidate
			}
		}
	}
	if _, ok := live[id]; !ok {
		fmt.Fprintf(w, `<%sResponse><%s></%s></%sResponse>`, act, set, set, act)
		return
	}
	fmt.Fprintf(w, `<%sResponse><%s><item><%s>%s</%s><cidrBlock>%s</cidrBlock></item></%s></%sResponse>`,
		act, set, item, id, item, cidrs[id], set, act)
}

func fakeEC2(t *testing.T, seen *[]string, tags *map[string]string) *httptest.Server {
	t.Helper()
	n := 0
	// id → correlation key, so Describe can answer both by id and by the tag filter.
	live := map[string]string{}
	cidrs := map[string]string{}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		act := r.PostForm.Get("Action")
		*seen = append(*seen, act)
		w.Header().Set("Content-Type", "text/xml")
		switch act {
		case "CreateVpc", "CreateSubnet":
			for i := 1; ; i++ {
				k := r.PostForm.Get(fmt.Sprintf("TagSpecification.1.Tag.%d.Key", i))
				if k == "" {
					break
				}
				(*tags)[act+":"+k] = r.PostForm.Get(fmt.Sprintf("TagSpecification.1.Tag.%d.Value", i))
			}
			n++
			corr := r.PostForm.Get("TagSpecification.1.Tag.1.Value")
			if act == "CreateVpc" {
				id := fmt.Sprintf("vpc-%03d", n)
				live[id], cidrs[id] = corr, r.PostForm.Get("CidrBlock")
				fmt.Fprintf(w, `<CreateVpcResponse><vpc><vpcId>%s</vpcId><cidrBlock>%s</cidrBlock></vpc></CreateVpcResponse>`, id, r.PostForm.Get("CidrBlock"))
				return
			}
			id := fmt.Sprintf("subnet-%03d", n)
			live[id], cidrs[id] = corr, r.PostForm.Get("CidrBlock")
			fmt.Fprintf(w, `<CreateSubnetResponse><subnet><subnetId>%s</subnetId><cidrBlock>%s</cidrBlock></subnet></CreateSubnetResponse>`, id, r.PostForm.Get("CidrBlock"))
		case "DescribeVpcs", "DescribeSubnets":
			describe(w, r, act, live, cidrs)
		default:
			http.Error(w, "unexpected "+act, http.StatusBadRequest)
		}
	}))
}

func TestNetworkProvisionAppliesBothStepsAndStampsTags(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIATEST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	var seen []string
	stamped := map[string]string{}
	srv := fakeEC2(t, &seen, &stamped)
	defer srv.Close()

	dep, err := omnisdk.AWSNetwork("demo", "us-east-1", "10.0.0.0/16", "10.0.1.0/24",
		map[string]string{"Name": "demo-vpc", "env": "dev"},
		map[string]string{"Name": "demo-subnet"})
	if err != nil {
		t.Fatalf("deployment: %v", err)
	}
	pl, err := omnisdk.Converge(dep, t.TempDir(), "run-1", omnisdk.Args{Endpoint: srv.URL + "/"})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	rows, err := pl.Open(context.Background())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer rows.Close()

	var got []omnisdk.Row
	for rows.Next() {
		got = append(got, rows.Row())
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("rows = %v, want one per applied key", got)
	}
	if got[0]["identity"] != "vpc-001" || got[1]["identity"] != "subnet-002" {
		t.Errorf("identities = %v / %v, want vpc-001 / subnet-002", got[0]["identity"], got[1]["identity"])
	}

	// Caller tags are stamped alongside the correlation key, which is what makes the objects
	// rediscoverable without the ledger.
	for k, want := range map[string]string{
		"CreateVpc:Name":        "demo-vpc",
		"CreateVpc:env":         "dev",
		"CreateSubnet:Name":     "demo-subnet",
		"CreateVpc:omnisdk:key": "demo/aws/ec2/vpc",
	} {
		if stamped[k] != want {
			t.Errorf("tag %s = %q, want %q", k, stamped[k], want)
		}
	}
}

// A second run over the same state converges: the ledger says both keys are live and the target
// agrees, so no create is issued.
func TestNetworkProvisionRerunIssuesNoCreates(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIATEST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	var seen []string
	stamped := map[string]string{}
	srv := fakeEC2(t, &seen, &stamped)
	defer srv.Close()

	state := t.TempDir()
	run := func(id string) {
		t.Helper()
		dep, err := omnisdk.AWSNetwork("demo", "us-east-1", "10.0.0.0/16", "10.0.1.0/24", nil, nil)
		if err != nil {
			t.Fatalf("deployment: %v", err)
		}
		pl, err := omnisdk.Converge(dep, state, id, omnisdk.Args{Endpoint: srv.URL + "/"})
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
	}
	run("run-1")
	before := strings.Count(strings.Join(seen, " "), "Create")
	run("run-2")

	if after := strings.Count(strings.Join(seen, " "), "Create"); after != before {
		t.Errorf("creates went from %d to %d on re-run, want no new calls", before, after)
	}
}
