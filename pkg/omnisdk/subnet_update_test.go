package omnisdk_test

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
)

func TestLiveSubnetExampleUpdatesThenConverges(t *testing.T) {
	requireCorpus(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIATEST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	registry := filepath.Join(t.TempDir(), "registry")
	cmd := exec.Command("python3", "../../cicd/examples/aws-vpc-subnet/prepare-update.py", "--registry", corpus, "--out", registry)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("prepare: %s %v", out, err)
	}
	var seen []string
	tags := map[string]string{}
	base := fakeEC2(t, &seen, &tags)
	defer base.Close()
	var mu sync.Mutex
	public := false
	name := ""
	labels := map[string]string{}
	writes := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if err := r.ParseForm(); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		action := param(r, "Action")
		switch action {
		case "CreateTags", "DeleteTags":
			id, key := param(r, "ResourceId.1"), param(r, "Tag.1.Key")
			if r.Method != "POST" || param(r, "VpcId") != "" || param(r, "CidrBlock") != "" {
				t.Errorf("tag request: %s %v", r.Method, r.Form)
			}
			if key == "Name" && id == "vpc-001" && action == "CreateTags" {
				name = param(r, "Tag.1.Value")
			} else if key == "resource-key" && (id == "vpc-001" || id == "subnet-002") {
				if action == "DeleteTags" {
					if param(r, "Tag.1.Value") != "" {
						t.Error("delete must address tag key")
					}
					delete(labels, id)
				} else {
					labels[id] = param(r, "Tag.1.Value")
				}
			} else {
				t.Errorf("unexpected tag request: %v", r.Form)
			}
			writes[action+":"+key]++
			w.Header().Set("Content-Type", "text/xml")
			fmt.Fprintf(w, "<%sResponse><return>true</return></%sResponse>", action, action)
			return
		case "ModifySubnetAttribute":
			if r.Method != "POST" || param(r, "SubnetId") != "subnet-002" || param(r, "VpcId") != "" || param(r, "CidrBlock") != "" {
				t.Errorf("update request: %s %v", r.Method, r.Form)
			}
			value := param(r, "MapPublicIpOnLaunch.Value")
			if value != "true" && value != "false" {
				t.Errorf("invalid boolean %q", value)
			}
			public = value == "true"
			writes[action]++
			w.Header().Set("Content-Type", "text/xml")
			fmt.Fprint(w, `<ModifySubnetAttributeResponse><return>true</return></ModifySubnetAttributeResponse>`)
			return
		}
		response := httptest.NewRecorder()
		base.Config.Handler.ServeHTTP(response, r)
		if action == "CreateVpc" {
			labels["vpc-001"] = tags["CreateVpc:resource-key"]
		}
		if action == "CreateSubnet" {
			labels["subnet-002"] = tags["CreateSubnet:resource-key"]
		}
		for k, v := range response.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(response.Code)
		body := response.Body.String()
		if action == "DescribeVpcs" || action == "DescribeSubnets" {
			id := "vpc-001"
			if action == "DescribeSubnets" {
				id = "subnet-002"
			}
			xml := `<tagSet><item><key>owner</key><value>someone-else</value></item>`
			if value, ok := labels[id]; ok {
				xml += `<item><key>resource-key</key><value>` + html.EscapeString(value) + `</value></item>`
			}
			if id == "vpc-001" && name != "" {
				xml += `<item><key>Name</key><value>` + html.EscapeString(name) + `</value></item>`
			}
			xml += `</tagSet>`
			if action == "DescribeSubnets" {
				xml += fmt.Sprintf("<mapPublicIpOnLaunch>%t</mapPublicIpOnLaunch>", public)
			}
			body = strings.ReplaceAll(body, "</cidrBlock>", "</cidrBlock>"+xml)
		}
		fmt.Fprint(w, body)
	}))
	defer server.Close()
	state := t.TempDir()
	run := func(id, file string, args omnisdk.Args) {
		t.Helper()
		path := file
		if !filepath.IsAbs(path) {
			path = "../../cicd/examples/aws-vpc-subnet/" + file
		}
		resources, err := omnisdk.LoadResources(path)
		if err != nil {
			t.Fatal(err)
		}
		args.Endpoint = server.URL
		if args.Params == nil {
			args.Params = map[string]string{"region": "us-east-1"}
		}
		p, err := omnisdk.Prepare(registry, omnisdk.DesiredState(resources), omnisdk.NewLedger("demo", state, id), args)
		if err != nil {
			t.Fatal(err)
		}
		rows, err := p.Open(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			if row := rows.Row(); row["error"] != nil {
				t.Fatalf("%s: %v", id, row)
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
	}
	run("create", "resources.json", omnisdk.Args{})
	run("create-again", "resources.json", omnisdk.Args{})
	run("enable", "update-public-ip.json", omnisdk.Args{})
	run("enable-again", "update-public-ip.json", omnisdk.Args{})
	run("disable", "update-private-ip.json", omnisdk.Args{})
	run("disable-again", "update-private-ip.json", omnisdk.Args{})
	// A reused create ID must not replay the journal and delete this existing network.
	resources, err := omnisdk.LoadResources("../../cicd/examples/aws-vpc-subnet/resources.json")
	if err != nil {
		t.Fatal(err)
	}
	replay, err := omnisdk.Prepare(registry, omnisdk.DesiredState(resources), omnisdk.NewLedger("demo", state, "create"), omnisdk.Args{Endpoint: server.URL, Params: map[string]string{"region": "us-east-1"}})
	if err != nil {
		t.Fatal(err)
	}
	before := len(seen)
	if rows, err := replay.Open(context.Background()); err == nil || !strings.Contains(err.Error(), "already exists") {
		if rows != nil {
			rows.Close()
		}
		t.Fatalf("reused create journal accepted: %v", err)
	}
	if len(seen) != before {
		t.Fatalf("reused run called the provider: %v", seen[before:])
	}
	run("still-converged", "update-private-ip.json", omnisdk.Args{})
	raw, err := os.ReadFile("../../cicd/examples/aws-vpc-subnet/update-private-ip.json")
	if err != nil {
		t.Fatal(err)
	}
	var spec map[string]any
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	vpc := spec["resources"].([]any)[0].(map[string]any)
	vpc["desired"].(map[string]any)["TagSpecification.1.Tag.1.Value"] = map[string]any{"from": "input", "attribute": "label"}
	vpc["write_once"].(map[string]any)["region"] = map[string]any{"from": "env", "attribute": "REGION"}
	write := func() string {
		t.Helper()
		raw, err := json.Marshal(spec)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "bound.json")
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	path := write()
	args := omnisdk.Args{Inputs: map[string]any{"label": "team & staging+new=1 / café"}, Environment: map[string]any{"REGION": "us-east-1"}}
	run("input-label", path, args)
	run("input-label-again", path, args)
	mu.Lock()
	if labels["vpc-001"] != args.Inputs["label"] || labels["subnet-002"] != "aws/ec2/subnet" {
		t.Errorf("labels=%v", labels)
	}
	mu.Unlock()
	delete(vpc["desired"].(map[string]any), "TagSpecification.1.Tag.1.Value")
	path = write()
	run("remove-label", path, args)
	run("remove-label-again", path, args)
	mu.Lock()
	defer mu.Unlock()
	if _, found := labels["vpc-001"]; found {
		t.Error("removed desired tag still present")
	}
	if name != "omni-demo-vpc" || public || writes["CreateTags:Name"] != 1 || writes["ModifySubnetAttribute"] != 2 || writes["CreateTags:resource-key"] != 1 || writes["DeleteTags:resource-key"] != 1 {
		t.Errorf("name=%q public=%v writes=%v", name, public, writes)
	}
	if got := strings.Count(strings.Join(seen, " "), "Create"); got != 2 {
		t.Errorf("infrastructure creates=%d", got)
	}
}
