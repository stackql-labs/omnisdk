package sdk

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Provision against a mock EC2 Query endpoint: CreateVpc returns a vpc id, which the β bind
// join must feed into CreateSubnet's VpcId; the output row carries both ids and both
// timestamped descriptions. No real AWS.
func TestProvisionBindsVpcIntoSubnet(t *testing.T) {
	const vpcID = "vpc-abc123"
	const subnetID = "subnet-def456"

	var gotSubnetVpcID string
	var sawVpcTag, sawSubnetTag string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		q := r.PostForm // EC2 Query API params ride the POST form body
		w.Header().Set("Content-Type", "application/xml")
		switch q.Get("Action") {
		case "CreateVpc":
			sawVpcTag = q.Get("TagSpecification.1.Tag.1.Value")
			_, _ = w.Write([]byte(`<CreateVpcResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/">` +
				`<requestId>r1</requestId><vpc><vpcId>` + vpcID + `</vpcId></vpc></CreateVpcResponse>`))
		case "CreateSubnet":
			gotSubnetVpcID = q.Get("VpcId")
			sawSubnetTag = q.Get("TagSpecification.1.Tag.1.Value")
			_, _ = w.Write([]byte(`<CreateSubnetResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/">` +
				`<requestId>r2</requestId><subnet><subnetId>` + subnetID + `</subnetId></subnet></CreateSubnetResponse>`))
		default:
			http.Error(w, "unexpected action", http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	creds := Credentials{AccessKeyID: "test", SecretAccessKey: "test"}
	var out bytes.Buffer
	op := NewProvision(1, "us-east-1", creds, srv.URL, "10.0.0.0/16", "10.0.1.0/24", &out)

	ctx := context.Background()
	rs := op.Open(ctx)
	defer rs.Close()
	for rs.Next(ctx) { //nolint // sink drives the pull
	}
	if err := rs.Err(); err != nil {
		t.Fatalf("provision stream err: %v", err)
	}

	// β: the subnet request must carry the VPC id created upstream.
	if gotSubnetVpcID != vpcID {
		t.Errorf("subnet VpcId = %q, want %q (β did not bind)", gotSubnetVpcID, vpcID)
	}

	line := strings.TrimSpace(out.String())
	var row map[string]any
	if err := json.Unmarshal([]byte(line), &row); err != nil {
		t.Fatalf("output not JSON (%v): %q", err, line)
	}
	if row["vpc_id"] != vpcID || row["subnet_id"] != subnetID {
		t.Errorf("row ids = %v/%v, want %s/%s", row["vpc_id"], row["subnet_id"], vpcID, subnetID)
	}
	// Both descriptions present, timestamped, and matching what went on the wire.
	vd, _ := row["vpc_description"].(string)
	sd, _ := row["subnet_description"].(string)
	if !strings.HasPrefix(vd, "omnisdk vpc ") || vd != sawVpcTag {
		t.Errorf("vpc_description = %q (wire tag %q)", vd, sawVpcTag)
	}
	if !strings.HasPrefix(sd, "omnisdk subnet ") || sd != sawSubnetTag {
		t.Errorf("subnet_description = %q (wire tag %q)", sd, sawSubnetTag)
	}
}

// A missing required κ input (vpc_cidr) must reject the plan instantly, before any AWS call.
func TestProvisionRejectsMissingCidr(t *testing.T) {
	creds := Credentials{AccessKeyID: "test", SecretAccessKey: "test"}
	var out bytes.Buffer
	op := NewProvision(1, "us-east-1", creds, "http://unused.invalid", "", "10.0.1.0/24", &out)

	ctx := context.Background()
	rs := op.Open(ctx)
	defer rs.Close()
	for rs.Next(ctx) { //nolint // draining to surface the terminal error
	}
	err := rs.Err()
	if err == nil {
		t.Fatal("expected instant rejection for missing vpc_cidr, got nil")
	}
	if !strings.Contains(err.Error(), "vpc_cidr") {
		t.Errorf("error = %v, want mention of vpc_cidr", err)
	}
	if out.Len() != 0 {
		t.Errorf("expected no output on rejection, got %q", out.String())
	}
}
