package stackqldoc

import (
	"bytes"
	"crypto/mldsa"
	"encoding/hex"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stackql/stackql-provider-registry/signing/pqsig"
)

func signedRegistry(t *testing.T) (fstest.MapFS, pqsig.Verifier, *mldsa.PrivateKey) {
	t.Helper()
	key, err := mldsa.GenerateKey(mldsa.MLDSA65())
	if err != nil {
		t.Fatal(err)
	}
	v, err := pqsig.NewVerifier(map[string]string{"test": hex.EncodeToString(key.PublicKey().Bytes())})
	if err != nil {
		t.Fatal(err)
	}
	fsys := fstest.MapFS{}
	for name, source := range map[string]string{
		"example/v1/provider.yaml":         "id: example\nname: example\nversion: v1\nproviderServices:\n  widgets:\n    id: widgets\n    name: widgets\n    preferred: true\n    service:\n      $ref: example/v1/services/widgets.yaml\n",
		"example/v1/services/widgets.yaml": "openapi: 3.0.0\ninfo: {title: widgets, version: v1}\npaths: {}\ncomponents:\n  x-stackQL-resources:\n    things: {id: example.widgets.things, name: things}\n",
		"example/v1/services/other.yaml":   "openapi: 3.0.0\ninfo: {title: other, version: v1}\npaths: {}\n",
	} {
		signed, err := pqsig.Sign([]byte(source), "test", key)
		if err != nil {
			t.Fatal(err)
		}
		fsys[name] = &fstest.MapFile{Data: signed}
	}
	return fsys, v, key
}

func TestVerifiedRegistry(t *testing.T) {
	fsys, v, _ := signedRegistry(t)
	r, err := OpenRegistry(fsys, WithVerifier(v))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Catalog("example"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.(*registry).ServiceFile("stackql_unstable_example.widgets"); err != nil {
		t.Fatal(err)
	}
	file := fsys["example/v1/services/widgets.yaml"]
	file.Data = bytes.Replace(file.Data, []byte("title: widgets"), []byte("title: changed"), 1)
	if _, err := r.(*registry).ServiceFile("stackql_unstable_example.widgets"); err == nil || !strings.Contains(err.Error(), "services/widgets.yaml") {
		t.Fatalf("tampered service was not rejected with path: %v", err)
	}
}

func TestVerifiedRegistryRejectsBadMetadata(t *testing.T) {
	for _, kind := range []string{"tampered", "unknown", "missing"} {
		t.Run(kind, func(t *testing.T) {
			fsys, v, _ := signedRegistry(t)
			file := fsys["example/v1/provider.yaml"]
			switch kind {
			case "tampered":
				file.Data = bytes.Replace(file.Data, []byte("name: example"), []byte("name: changed"), 1)
			case "unknown":
				file.Data = bytes.Replace(file.Data, []byte("kid: test"), []byte("kid: unknown"), 1)
			case "missing":
				file.Data = []byte("name: example\n")
			}
			if _, err := OpenRegistry(fsys, WithVerifier(v)); err == nil || !strings.Contains(err.Error(), "example/v1/provider.yaml") {
				t.Fatalf("bad metadata was not rejected with path: %v", err)
			}
			if _, err := OpenRegistry(fsys); err != nil {
				t.Fatalf("verification became mandatory: %v", err)
			}
		})
	}
	if _, err := OpenRegistry(fstest.MapFS{}, WithVerifier(nil)); err == nil {
		t.Fatal("nil verifier disabled verification")
	}
}

func TestVerifiedServiceAndReferenceReads(t *testing.T) {
	fsys, v, _ := signedRegistry(t)
	bundle := fstest.MapFS{
		"provider.yaml":         fsys["example/v1/provider.yaml"],
		"services/widgets.yaml": fsys["example/v1/services/widgets.yaml"],
		"services/other.yaml":   fsys["example/v1/services/other.yaml"],
	}
	unverified, err := Open(bundle)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := unverified.(*catalog).doc("widgets")
	if err != nil {
		t.Fatal(err)
	}
	cache := &verificationPoisonCache{doc: parsed}
	verified, err := Open(bundle, WithVerifier(v), WithDocCache(cache, "same-root"))
	if err != nil {
		t.Fatal(err)
	}
	c := verified.(*catalog)
	if _, err := c.doc("widgets"); err != nil {
		t.Fatal(err)
	}
	ref := bundleResolver{catalog: c}
	if _, err := ref.ResolveDocument("services/other.yaml", "services/widgets.yaml"); err != nil {
		t.Fatal(err)
	}
	bundle["services/widgets.yaml"].Data = []byte("openapi: 3.0.0\n")
	bundle["services/other.yaml"].Data = []byte("openapi: 3.0.0\n")
	if _, err := c.doc("widgets"); err == nil {
		t.Fatal("unsigned service or cached unverified parse was accepted")
	}
	if _, err := ref.ResolveDocument("services/other.yaml", "services/widgets.yaml"); err == nil {
		t.Fatal("unsigned external reference was accepted")
	}
	if cache.read {
		t.Fatal("verified reader used an unverified shared parse cache")
	}
}

type verificationPoisonCache struct {
	doc  Doc
	read bool
}

func (c *verificationPoisonCache) Get(string) (Doc, bool) { c.read = true; return c.doc, true }
func (*verificationPoisonCache) Put(string, Doc, int64)   {}
