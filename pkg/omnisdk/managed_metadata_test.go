package omnisdk

import (
	"encoding/json"
	"testing"
)

func TestManagedMetadataPreservesIndexedDeleteInputs(t *testing.T) {
	d := durable{provider: "aws", address: "stackql_unstable_aws.ec2.ingress", resource: NewManaged("ingress", []string{"create"}, "id", "GroupId"), deletes: map[string]bool{"GroupId": true, "IpPermissions": true}}
	data := d.aboutOf(map[string]any{"GroupId": "sg-web", "IpPermissions.1.FromPort": "22", "IpPermissions.1.IpRanges.1.CidrIp": "203.0.113.7/32", "unrelated": "not-delete-input"})
	var record aboutRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	if record.Params["IpPermissions.1.FromPort"] != "22" || record.Params["IpPermissions.1.IpRanges.1.CidrIp"] != "203.0.113.7/32" {
		t.Fatalf("delete params=%v", record.Params)
	}
	if _, found := record.Params["unrelated"]; found {
		t.Fatal("stored unrelated value")
	}
}
