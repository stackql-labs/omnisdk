package httpx

import (
	"testing"

	"github.com/stackql-labs/omnisdk/internal/system_g/bind"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/transform"
)

// tuple builds a bind-join {input, output} record for the flatten under test.
func tuple(input, output map[string]any) facade.Record {
	m := map[string]any{bind.KeyInput: input}
	if output != nil {
		m[bind.KeyOutput] = output
	}
	return bind.NewDocRecord(m)
}

// NewExtract with a dotted out-key builds a nested object, an absent path is omitted (optional),
// and literals merge onto the input row — all encoding-agnostic (here, XML via the injected mxj
// decoder). This is exactly what the S3 GetBucketEncryption flatten needs, as config only.
func TestExtractNestedOptionalXML(t *testing.T) {
	const sse = `<ServerSideEncryptionConfiguration><Rule>` +
		`<ApplyServerSideEncryptionByDefault><SSEAlgorithm>AES256</SSEAlgorithm></ApplyServerSideEncryptionByDefault>` +
		`<BucketKeyEnabled>false</BucketKeyEnabled></Rule></ServerSideEncryptionConfiguration>`

	ex := NewExtract(transform.NewXMLToAgnostic(), map[string]string{
		"encryption_status":             "ServerSideEncryptionConfiguration.Rule.ApplyServerSideEncryptionByDefault.SSEAlgorithm",
		"encryption.algorithm":          "ServerSideEncryptionConfiguration.Rule.ApplyServerSideEncryptionByDefault.SSEAlgorithm",
		"encryption.kms_key_id":         "ServerSideEncryptionConfiguration.Rule.ApplyServerSideEncryptionByDefault.KMSMasterKeyID",
		"encryption.bucket_key_enabled": "ServerSideEncryptionConfiguration.Rule.BucketKeyEnabled",
	}, nil)

	rec, err := ex.Apply(tuple(map[string]any{"name": "alpha"}, map[string]any{"status": "200", bind.KeyRaw: sse}))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	m, _ := bind.DocMap(rec)
	if m["name"] != "alpha" || m["encryption_status"] != "AES256" {
		t.Fatalf("row = %v, want name alpha + AES256", m)
	}
	enc, ok := m["encryption"].(map[string]any)
	if !ok {
		t.Fatalf("encryption not a nested object: %v", m["encryption"])
	}
	if enc["algorithm"] != "AES256" || enc["bucket_key_enabled"] != "false" {
		t.Errorf("encryption = %v, want algorithm AES256 + bucket_key_enabled false", enc)
	}
	if _, present := enc["kms_key_id"]; present {
		t.Errorf("kms_key_id should be omitted (absent path), got %v", enc["kms_key_id"])
	}
}

// NewStatusBranch dispatches on the response code: 200 runs its extract, 404 a literal default,
// and an unmatched status with a nil default fails the flow — the canonical E_α behavioural edge.
func TestStatusBranch(t *testing.T) {
	br := NewStatusBranch(map[string]facade.Transform{
		"200": NewExtract(nil, map[string]string{"id": "vpcId"}, nil),
		"404": NewExtract(nil, nil, map[string]any{"state": "none"}),
	}, nil)

	ok, err := br.Apply(tuple(map[string]any{"name": "a"}, map[string]any{"status": "200", bind.KeyRaw: `{"vpcId":"vpc-1"}`}))
	if err != nil {
		t.Fatalf("200: %v", err)
	}
	if m, _ := bind.DocMap(ok); m["id"] != "vpc-1" || m["name"] != "a" {
		t.Errorf("200 row = %v, want id vpc-1 + name a", m)
	}

	none, err := br.Apply(tuple(map[string]any{"name": "b"}, map[string]any{"status": "404", bind.KeyRaw: ``}))
	if err != nil {
		t.Fatalf("404: %v", err)
	}
	if m, _ := bind.DocMap(none); m["state"] != "none" {
		t.Errorf("404 row = %v, want state none", m)
	}

	if _, err := br.Apply(tuple(map[string]any{}, map[string]any{"status": "500", bind.KeyRaw: `boom`})); err == nil {
		t.Error("unmatched status with nil default must error")
	}
}
