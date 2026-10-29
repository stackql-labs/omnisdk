package omnisdk_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
)

// A resources file is a list of resources, or an object holding one — an omnicli spec — with each
// field in either spelling the callers already use.
func TestResourcesFile(t *testing.T) {
	for name, body := range map[string]string{
		"write_once": `[{"key":"a","provider":"aws","address":"s3.buckets","desired":{"x":1},
			"write_once":{"bucket":"b"},"inbound":[{"from":"z","as":"bucket"}],"identity":"Name","addressed_by":"bucket"}]`,
		"list, snake_case": `[{"key":"a","provider":"aws","address":"s3.buckets","desired":{"x":1},
			"write_once":{"bucket":"b"},"inbound":[{"from":"z","as":"bucket"}],"identity":"Name","addressed_by":"bucket"}]`,
		"object, camelCase": `{"name":"ignored","resources":[{"key":"a","provider":"aws","address":"s3.buckets","desired":{"x":1},
			"write_once":{"bucket":"b"},"inbound":[{"from":"z","as":"bucket"}],"identity":"Name","addressedBy":"bucket"}]}`,
	} {
		path := filepath.Join(t.TempDir(), "spec.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		rs, err := omnisdk.LoadResources(path)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		r := rs[0]
		if len(rs) != 1 || r.Key() != "a" || r.Address() != "s3.buckets" || r.AddressedBy() != "bucket" ||
			r.Identity() != "Name" || r.Params()["bucket"] != "b" || string(r.Desired()) != `{"x":1}` ||
			len(r.Inbound()) != 1 || r.Inbound()[0].As != "bucket" {
			t.Errorf("%s: resource = %+v", name, r)
		}
	}
	if _, err := omnisdk.ParseResources([]byte(`{"nope":1}`)); err == nil {
		t.Error("accepted a document holding no resources")
	}
	for _, body := range []string{
		`[{"key":"a","params":{}}]`,
		`[{"key":"a","params":{},"write_once":{}}]`,
	} {
		if _, err := omnisdk.ParseResources([]byte(body)); err == nil {
			t.Error("accepted removed params field")
		}
	}
}
