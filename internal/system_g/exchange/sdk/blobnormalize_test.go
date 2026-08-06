package sdk

import (
	"testing"

	"github.com/stackql-labs/omnisdk/internal/system_g/bind"
)

// A real bucket is classified; a left-outer "scope with no bucket" (no name) must NOT fabricate
// attributes — every attribute stays null, so an empty project can never masquerade as a public bucket.
func TestBlobNormalizeNoResourceIsAllNull(t *testing.T) {
	// no-bucket row (left-outer): only the scope + provider are present, no name.
	rec, err := blobNormalize{}.Apply(bind.NewDocRecord(map[string]any{"provider": "gcp"}))
	if err != nil {
		t.Fatal(err)
	}
	m, _ := bind.DocMap(rec)
	for _, k := range []string{"name", "encryption_status", "encryption_class", "public", "versioning", "https"} {
		if m[k] != nil {
			t.Errorf("no-resource row: %s = %v, want nil", k, m[k])
		}
	}
	if m["provider"] != "gcp" {
		t.Errorf("provider = %v, want gcp", m["provider"])
	}

	// real bucket: classified normally.
	rec, err = blobNormalize{}.Apply(bind.NewDocRecord(map[string]any{
		"provider": "gcp", "name": "b1", "encryption_status": "projects/p/.../k", "public": "enforced",
	}))
	if err != nil {
		t.Fatal(err)
	}
	m, _ = bind.DocMap(rec)
	if m["encryption_class"] != "customer-managed" || m["public"] != false {
		t.Errorf("real bucket classified wrong: %v", m)
	}
}
