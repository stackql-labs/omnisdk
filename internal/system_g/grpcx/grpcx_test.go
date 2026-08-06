package grpcx_test

import (
	"context"
	"testing"

	"github.com/stackql-labs/omnisdk/internal/system_g/bind"
	"github.com/stackql-labs/omnisdk/internal/system_g/grpcx"
	"github.com/stackql-labs/omnisdk/internal/system_g/grpcx/grpctest"
)

// TestDynamicRoundTrip proves the transport end to end with NO generated code on either side: the
// server serves the embedded proto dynamically (grpctest/dynamicpb), and the grpcurl-based client
// invokes it purely from descriptors, emitting each response as an agnostic doc the plan can read.
func TestDynamicRoundTrip(t *testing.T) {
	target, dialOpts, stop, err := grpctest.NewStorageServer([]grpctest.Bucket{
		{Name: "gcs-plain", PublicAccessPrevention: "inherited"},
		{Name: "gcs-cmek", KMSKey: "projects/p/locations/l/keyRings/r/cryptoKeys/k", PublicAccessPrevention: "enforced", Versioning: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	d, err := grpcx.Load()
	if err != nil {
		t.Fatal(err)
	}
	factory := grpcx.Make(d, grpcx.Request{
		Target:       target,
		Method:       "google.storage.v2.Storage.ListBuckets",
		Fields:       map[string]any{"parent": "projects/{project}", "page_size": 100},
		Metadata:     map[string]string{"authorization": "Bearer {token}"},
		Continuation: grpcx.Continuation{PageTokenField: "page_token", NextTokenPath: "nextPageToken"},
	}, dialOpts...)

	rs := factory(map[string]any{"project": "demo", "token": "tok"}).Open(context.Background())
	defer rs.Close()

	var docs []map[string]any
	for rs.Next(context.Background()) {
		m, _ := bind.DocMap(rs.Record())
		docs = append(docs, m)
	}
	if err := rs.Err(); err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 {
		t.Fatalf("got %d response pages, want 1", len(docs))
	}
	buckets, _ := docs[0]["buckets"].([]any)
	if len(buckets) != 2 {
		t.Fatalf("got %d buckets, want 2: %v", len(buckets), docs[0])
	}
	cmek := findBucket(buckets, "gcs-cmek")
	if cmek == nil {
		t.Fatalf("gcs-cmek not found: %v", buckets)
	}
	// Descriptor-driven JSON serde: nested messages surface as nested maps (camelCase JSON names).
	if enc, _ := cmek["encryption"].(map[string]any); enc["defaultKmsKey"] == nil || enc["defaultKmsKey"] == "" {
		t.Errorf("encryption.defaultKmsKey missing: %v", cmek)
	}
	if iam, _ := cmek["iamConfig"].(map[string]any); iam["publicAccessPrevention"] != "enforced" {
		t.Errorf("iamConfig.publicAccessPrevention = %v, want enforced", cmek["iamConfig"])
	}
	if ver, _ := cmek["versioning"].(map[string]any); ver["enabled"] != true {
		t.Errorf("versioning.enabled = %v, want true", cmek["versioning"])
	}
}

func findBucket(buckets []any, bucketID string) map[string]any {
	for _, b := range buckets {
		if m, ok := b.(map[string]any); ok && m["bucketId"] == bucketID {
			return m
		}
	}
	return nil
}
