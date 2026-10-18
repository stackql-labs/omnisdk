package omnisdk_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
)

// tenancyEC2 is one VPC's worth of EC2: create, describe by id or by correlation tag, and change
// tenancy. drift changes the tenancy behind the tool's back, as a console edit would.
type tenancyEC2 struct {
	mu      sync.Mutex
	calls   []string
	id      string
	corr    string
	cidr    string
	tenancy string
}

func (e *tenancyEC2) drift(tenancy string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.tenancy = tenancy
}

func (e *tenancyEC2) took() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := strings.Join(e.calls, ",")
	e.calls = nil
	return out
}

func (e *tenancyEC2) serve(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		act := param(r, "Action")
		e.calls = append(e.calls, act)
		w.Header().Set("Content-Type", "text/xml")
		switch act {
		case "CreateVpc":
			e.id, e.corr, e.cidr = "vpc-001", param(r, "TagSpecification.1.Tag.1.Value"), param(r, "CidrBlock")
			e.tenancy = param(r, "InstanceTenancy")
			fmt.Fprintf(w, `<CreateVpcResponse><vpc><vpcId>%s</vpcId></vpc></CreateVpcResponse>`, e.id)
		case "DescribeVpcs":
			match := e.id != "" && (param(r, "VpcId.1") == e.id || param(r, "Filter.1.Value.1") == e.corr)
			if !match {
				fmt.Fprint(w, `<DescribeVpcsResponse><vpcSet></vpcSet></DescribeVpcsResponse>`)
				return
			}
			fmt.Fprintf(w, `<DescribeVpcsResponse><vpcSet><item><vpcId>%s</vpcId><cidrBlock>%s</cidrBlock>`+
				`<instanceTenancy>%s</instanceTenancy></item></vpcSet></DescribeVpcsResponse>`, e.id, e.cidr, e.tenancy)
		case "ModifyVpcTenancy":
			if param(r, "VpcId") != e.id || param(r, "CidrBlock") != "" || param(r, "TagSpecification.1.Tag.1.Value") != "" {
				http.Error(w, "ModifyVpcTenancy takes the VPC and its tenancy, nothing else", http.StatusBadRequest)
				return
			}
			e.tenancy = param(r, "InstanceTenancy")
			fmt.Fprint(w, `<ModifyVpcTenancyResponse><returnValue>true</returnValue></ModifyVpcTenancyResponse>`)
		default:
			http.Error(w, "unexpected "+act, http.StatusBadRequest)
		}
	}))
}

// Example: a VPC converged from desired state. The first run creates it; a rerun with nothing changed
// reads it and calls nothing else; a tenancy changed outside the tool is put back by the one
// operation the document says changes tenancy, sent only the field that differs.
func TestExampleVPCConvergesAndCorrectsDrift(t *testing.T) {
	requireCorpus(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIATEST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	ec2 := &tenancyEC2{}
	srv := ec2.serve(t)
	defer srv.Close()
	state := t.TempDir()
	vpc := []omnisdk.ManagedResource{omnisdk.NewResource("aws/ec2/vpc", "aws", "ec2.vpcs",
		[]byte(`{"CidrBlock":"10.42.0.0/16","InstanceTenancy":"default"}`),
		map[string]string{"TagSpecification.1.ResourceType": "vpc", "TagSpecification.1.Tag.1.Key": "omnisdk:key"},
		nil, "", "", "line_items.VpcId", "VpcId", "TagSpecification.1.Tag.1.Value")}

	converge := func(runID string) []omnisdk.Row {
		t.Helper()
		pl, err := omnisdk.Prepare(corpus, omnisdk.DesiredState(vpc), omnisdk.NewLedger("demo", state, runID), awsArgs(srv))
		if err != nil {
			t.Fatalf("prepare: %v", err)
		}
		rows := drain(t, pl)
		for _, row := range rows {
			if row["error"] != nil {
				t.Fatalf("%s: %v", runID, row)
			}
		}
		return rows
	}

	converge("r1")
	// No identity is recorded yet, so there is nothing to read: the first run only creates.
	if got := ec2.took(); got != "CreateVpc" {
		t.Errorf("first run: %s, want a create", got)
	}
	converge("r2")
	if got := ec2.took(); got != "DescribeVpcs" {
		t.Errorf("rerun: %s, want only a read", got)
	}
	ec2.drift("dedicated")
	converge("r3")
	if got := ec2.took(); got != "DescribeVpcs,ModifyVpcTenancy" {
		t.Errorf("after drift: %s, want a read and the tenancy change", got)
	}
	if ec2.tenancy != "default" {
		t.Errorf("tenancy = %s, want it put back to default", ec2.tenancy)
	}
}

// A failed run restores what it updated rather than deleting it: w1 existed before the run, so
// undoing the run's update puts its prior intent back, and the object stays.
func TestFailedRunRestoresAnUpdate(t *testing.T) {
	store := &widgetStore{items: map[string]map[string]any{}}
	srv := store.serve(t)
	defer srv.Close()
	state := t.TempDir()
	widget := func(key, size string) omnisdk.ManagedResource {
		return omnisdk.NewResource(key, "widgets", "things.widgets", []byte(fmt.Sprintf(`{"size":%q,"color":"red"}`, size)),
			nil, nil, "", "", "id", "id", "")
	}
	run := func(runID string, rs ...omnisdk.ManagedResource) []omnisdk.Row {
		t.Helper()
		pl, err := omnisdk.Prepare(authRegistry, omnisdk.DesiredState(rs), omnisdk.NewLedger("demo", state, runID), omnisdk.Args{Endpoint: srv.URL})
		if err != nil {
			t.Fatalf("prepare: %v", err)
		}
		return drain(t, pl)
	}
	run("r1", widget("w1", "L"))
	if got := strings.Join(store.took(), ","); got != "create" {
		t.Fatalf("first run: %s, want a create", got)
	}
	rows := run("r2", widget("w1", "M"), widget("w2", "bad"))
	last := rows[len(rows)-1]
	if !strings.HasPrefix(fmt.Sprint(last["status"]), "failed") || last["key"] != "demo/w2" {
		t.Fatalf("second run = %v, want it failed on w2", rows)
	}
	if len(store.items) != 1 || store.items["w-1"]["size"] != "L" {
		t.Errorf("store = %v, want w1 kept and its size restored to L", store.items)
	}
}
