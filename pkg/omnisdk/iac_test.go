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

// param reads a request value from wherever it arrived. The document declares GET with query
// parameters; a hand-authored plan used POST form. A stand-in that reads only one of them reports a
// wire change as a provider error.
func param(r *http.Request, name string) string {
	if v := r.URL.Query().Get(name); v != "" {
		return v
	}
	return r.PostForm.Get(name)
}

// describe answers a read either by id or by the correlation tag filter, which is what lets an
// object be adopted when the ledger no longer knows about it.
func describe(w http.ResponseWriter, r *http.Request, act string, live, cidrs map[string]string) {
	set, item, idAttr := "vpcSet", "vpcId", "VpcId.1"
	if act == "DescribeSubnets" {
		set, item, idAttr = "subnetSet", "subnetId", "SubnetId.1"
	}
	id := param(r, idAttr)
	if want := param(r, "Filter.1.Value.1"); want != "" {
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
		// The document declares GET with query parameters; the hand-written effector used POST form.
		// Reading both keeps the stand-in honest about what actually arrives.
		act := param(r, "Action")
		*seen = append(*seen, act)
		w.Header().Set("Content-Type", "text/xml")
		switch act {
		case "CreateVpc", "CreateSubnet", "CreateSecurityGroup":
			for i := 1; ; i++ {
				k := param(r, fmt.Sprintf("TagSpecification.1.Tag.%d.Key", i))
				if k == "" {
					break
				}
				(*tags)[act+":"+k] = param(r, fmt.Sprintf("TagSpecification.1.Tag.%d.Value", i))
			}
			n++
			corr := param(r, "TagSpecification.1.Tag.1.Value")
			if act == "CreateVpc" {
				id := fmt.Sprintf("vpc-%03d", n)
				live[id], cidrs[id] = corr, param(r, "CidrBlock")
				fmt.Fprintf(w, `<CreateVpcResponse><vpc><vpcId>%s</vpcId><cidrBlock>%s</cidrBlock></vpc></CreateVpcResponse>`, id, param(r, "CidrBlock"))
				return
			}
			if act == "CreateSecurityGroup" {
				id := fmt.Sprintf("sg-%03d", n)
				live[id] = corr
				fmt.Fprintf(w, `<CreateSecurityGroupResponse><groupId>%s</groupId></CreateSecurityGroupResponse>`, id)
				return
			}
			id := fmt.Sprintf("subnet-%03d", n)
			live[id], cidrs[id] = corr, param(r, "CidrBlock")
			fmt.Fprintf(w, `<CreateSubnetResponse><subnet><subnetId>%s</subnetId><cidrBlock>%s</cidrBlock></subnet></CreateSubnetResponse>`, id, param(r, "CidrBlock"))
		case "DescribeVpcs", "DescribeSubnets":
			describe(w, r, act, live, cidrs)
		case "DescribeSecurityGroups":
			fmt.Fprint(w, `<DescribeSecurityGroupsResponse><securityGroupInfo></securityGroupInfo></DescribeSecurityGroupsResponse>`)
		default:
			http.Error(w, "unexpected "+act, http.StatusBadRequest)
		}
	}))
}

// render resolves the published blueprint, which is how a client reaches a deployment: name the
// handle, supply inputs, converge under a collection name.
func render(t *testing.T, inputs map[string]string) []omnisdk.ManagedResource {
	t.Helper()
	bp, ok := omnisdk.BlueprintFor("aws-vpc-subnet")
	if !ok {
		t.Fatal("aws-vpc-subnet not published")
	}
	resources, err := bp.Resources(inputs)
	if err != nil {
		t.Fatalf("resources: %v", err)
	}
	return resources
}

func awsArgs(srv *httptest.Server) omnisdk.Args {
	return omnisdk.Args{Endpoint: srv.URL + "/", Params: map[string]string{"region": "us-east-1"}}
}

func TestNetworkProvisionAppliesBothStepsAndStampsTags(t *testing.T) {
	requireCorpus(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIATEST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	var seen []string
	stamped := map[string]string{}
	srv := fakeEC2(t, &seen, &stamped)
	defer srv.Close()

	resources := render(t, map[string]string{
		"region": "us-east-1", "vpc_cidr": "10.0.0.0/16", "subnet_cidr": "10.0.1.0/24",
		"vpc_tags":    `{"Name":"demo-vpc","env":"dev"}`,
		"subnet_tags": `{"Name":"demo-subnet"}`,
	})
	pl, err := omnisdk.Converge(corpus, "demo", t.TempDir(), "run-1", resources, awsArgs(srv))
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
	requireCorpus(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIATEST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	var seen []string
	stamped := map[string]string{}
	srv := fakeEC2(t, &seen, &stamped)
	defer srv.Close()

	state := t.TempDir()
	run := func(id string) {
		t.Helper()
		resources := render(t, map[string]string{
			"region": "us-east-1", "vpc_cidr": "10.0.0.0/16", "subnet_cidr": "10.0.1.0/24",
		})
		pl, err := omnisdk.Converge(corpus, "demo", state, id, resources, awsArgs(srv))
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

// Unknown or missing inputs fail at the blueprint rather than silently doing nothing: a mistyped
// parameter that has no effect is worse than one that errors.
func TestBlueprintRejectsBadInputs(t *testing.T) {
	bp, _ := omnisdk.BlueprintFor("aws-vpc-subnet")

	if _, err := bp.Resources(map[string]string{
		"region": "us-east-1", "vpc_cidr": "10.0.0.0/16", "subnet_cidr": "10.0.1.0/24",
		"vpc_tagz": `{"Name":"typo"}`,
	}); err == nil {
		t.Error("unknown input accepted, want an error")
	}

	if _, err := bp.Resources(map[string]string{"region": "us-east-1"}); err == nil {
		t.Error("missing required input accepted, want an error")
	}

	if _, err := bp.Resources(map[string]string{
		"region": "us-east-1", "vpc_cidr": "10.0.0.0/16", "subnet_cidr": "10.0.1.0/24",
		"vpc_tags": `not json`,
	}); err == nil {
		t.Error("malformed tags accepted, want an error")
	}
}

// A blueprint is written once and applied under any collection name: the name qualifies both the
// keys and the bindings between them.
func TestSameBlueprintUnderTwoNames(t *testing.T) {
	requireCorpus(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIATEST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	var seen []string
	stamped := map[string]string{}
	srv := fakeEC2(t, &seen, &stamped)
	defer srv.Close()

	state := t.TempDir()
	inputs := map[string]string{"region": "us-east-1", "vpc_cidr": "10.0.0.0/16", "subnet_cidr": "10.0.1.0/24"}
	for _, name := range []string{"alpha", "beta"} {
		pl, err := omnisdk.Converge(corpus, name, state, "run-"+name, render(t, inputs), awsArgs(srv))
		if err != nil {
			t.Fatalf("%s plan: %v", name, err)
		}
		rows, err := pl.Open(context.Background())
		if err != nil {
			t.Fatalf("%s open: %v", name, err)
		}
		var keys []string
		for rows.Next() {
			keys = append(keys, rows.Row()["key"].(string))
		}
		rows.Close()
		if len(keys) != 2 || keys[0] != name+"/aws/ec2/vpc" {
			t.Errorf("%s keys = %v, want them qualified by the collection name", name, keys)
		}
	}

	// Two collections, two independent sets of resources — the second did not adopt the first's.
	if n := strings.Count(strings.Join(seen, " "), "CreateVpc"); n != 2 {
		t.Errorf("CreateVpc issued %d times, want one per collection", n)
	}
}
