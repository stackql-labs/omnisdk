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
		t.Fatalf("prepare example: %s: %v", out, err)
	}
	var seen []string
	tags := map[string]string{}
	base := fakeEC2(t, &seen, &tags)
	defer base.Close()
	var mu sync.Mutex
	public := false
	missing := false
	modifications := 0
	name := ""
	nameUpdates := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if err := r.ParseForm(); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		action := param(r, "Action")
		if action == "CreateTags" {
			if r.Method != "POST" || param(r, "ResourceId.1") != "vpc-001" || param(r, "Tag.1.Key") != "Name" || param(r, "VpcId") != "" || param(r, "CidrBlock") != "" {
				t.Errorf("tag request: %s %v", r.Method, r.Form)
			}
			name = param(r, "Tag.1.Value")
			if name == "" {
				t.Error("empty name sent")
			}
			nameUpdates++
			w.Header().Set("Content-Type", "text/xml")
			fmt.Fprint(w, `<CreateTagsResponse><return>true</return></CreateTagsResponse>`)
			return
		}
		if action == "ModifySubnetAttribute" {
			if r.Method != "POST" || param(r, "SubnetId") != "subnet-002" || param(r, "VpcId") != "" || param(r, "CidrBlock") != "" {
				t.Errorf("update request: %s %v", r.Method, r.Form)
			}
			value := param(r, "MapPublicIpOnLaunch.Value")
			if value != "true" && value != "false" {
				t.Errorf("public-IP input = %q", value)
			}
			public = value == "true"
			modifications++
			w.Header().Set("Content-Type", "text/xml")
			fmt.Fprint(w, `<ModifySubnetAttributeResponse><return>true</return></ModifySubnetAttributeResponse>`)
			return
		}
		recorded := httptest.NewRecorder()
		base.Config.Handler.ServeHTTP(recorded, r)
		for k, v := range recorded.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(recorded.Code)
		body := recorded.Body.String()
		if action == "DescribeVpcs" {
			tagsXML := `<tagSet><item><key>omnisdk:key</key><value>demo/aws/ec2/vpc</value></item><item><key>owner</key><value>someone-else</value></item>`
			if name != "" {
				tagsXML += `<item><key>Name</key><value>` + html.EscapeString(name) + `</value></item>`
			}
			body = strings.ReplaceAll(body, "</cidrBlock>", "</cidrBlock>"+tagsXML+`</tagSet>`)
		}
		if action == "DescribeSubnets" {
			body = strings.ReplaceAll(body, "</cidrBlock>", fmt.Sprintf("</cidrBlock><mapPublicIpOnLaunch>%t</mapPublicIpOnLaunch>", public))
			if missing {
				body = `<DescribeSubnetsResponse><subnetSet/></DescribeSubnetsResponse>`
			}
		}
		fmt.Fprint(w, body)
	}))
	defer srv.Close()
	state := t.TempDir()
	run := func(id, file string, expectedError ...string) {
		t.Helper()
		path := file
		if !filepath.IsAbs(path) {
			path = "../../cicd/examples/aws-vpc-subnet/" + file
		}
		resources, err := omnisdk.LoadResources(path)
		if err != nil {
			t.Fatal(err)
		}
		p, err := omnisdk.Prepare(registry, omnisdk.DesiredState(resources), omnisdk.NewLedger("demo", state, id), awsArgs(srv))
		if err != nil {
			t.Fatal(err)
		}
		rows, err := p.Open(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var failed string
		for rows.Next() {
			if row := rows.Row(); row["error"] != nil {
				failed = fmt.Sprint(row["error"])
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if len(expectedError) > 0 {
			if !strings.Contains(failed, expectedError[0]) {
				t.Fatalf("%s: error=%q, expected %q", id, failed, expectedError[0])
			}
		} else if failed != "" {
			t.Fatalf("%s: %s", id, failed)
		}
	}
	run("create", "resources.json")
	run("enable", "update-public-ip.json")
	run("enable-again", "update-public-ip.json")
	mu.Lock()
	if !public || modifications != 1 {
		t.Errorf("public=%v modifications=%d", public, modifications)
	}
	if name != "omni-demo-vpc" || nameUpdates != 1 {
		t.Errorf("name=%q updates=%d", name, nameUpdates)
	}
	mu.Unlock()
	run("disable", "update-private-ip.json")
	run("disable-again", "update-private-ip.json")
	// A subsequent source edit and external drift must both reconcile, including form/XML escaping.
	raw, err := os.ReadFile("../../cicd/examples/aws-vpc-subnet/update-private-ip.json")
	if err != nil {
		t.Fatal(err)
	}
	var spec map[string]any
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	renamed := "omni & team+staging=1 / <new> café"
	spec["resources"].([]any)[0].(map[string]any)["desired"].(map[string]any)["Name"] = renamed
	raw, err = json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "rename.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	run("rename", path)
	run("rename-again", path)
	mu.Lock()
	if name != renamed || nameUpdates != 2 {
		t.Errorf("renamed=%q updates=%d", name, nameUpdates)
	}
	name = "external-drift"
	mu.Unlock()
	run("repair-drift", path)
	run("repair-drift-again", path)
	mu.Lock()
	if name != renamed || nameUpdates != 3 {
		t.Errorf("drift repair=%q updates=%d", name, nameUpdates)
	}
	missing = true
	mu.Unlock()
	run("missing", path, "require an existing object")
	mu.Lock()
	defer mu.Unlock()
	if public || modifications != 2 {
		t.Errorf("public=%v modifications=%d", public, modifications)
	}
	if got := strings.Count(strings.Join(seen, " "), "Create"); got != 2 {
		t.Errorf("create calls=%d, want only initial VPC and subnet", got)
	}
}
