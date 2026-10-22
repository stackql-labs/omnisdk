package auth

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stackql-labs/omnisdk/internal/system_g/httpx"
)

func TestOCIRequestSignaturesVerifyForGetAndBodyVerbs(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	cfg := AuthStruct{Type: string(KindOCI), TenancyOCID: "tenancy", UserOCID: "user", Fingerprint: "fp", PrivateKey: string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))}
	method, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, verb := range []string{"GET", "DELETE", "POST", "PUT", "PATCH"} {
		t.Run(verb, func(t *testing.T) {
			body := []byte(nil)
			if verb == "POST" || verb == "PUT" || verb == "PATCH" {
				body = []byte(`{"name":"café"}`)
			}
			in := httpx.NewRequestRecord(verb, "https://example.test:8443/a%2Fb?q=a%2Bb", http.Header{"X-Date": []string{"stale"}}, body)
			out, err := method.RequestTransform().Apply(in)
			if err != nil {
				t.Fatal(err)
			}
			hdr := httpx.Header(out)
			authorization := hdr.Get("Authorization")
			captures := regexp.MustCompile(`([A-Za-z]+)="([^"]*)"`).FindAllStringSubmatch(authorization, -1)
			fields := map[string]string{}
			for _, pair := range captures {
				fields[pair[1]] = pair[2]
			}
			if fields["keyId"] != "tenancy/user/fp" || fields["algorithm"] != "rsa-sha256" || fields["version"] != "1" {
				t.Fatalf("bad authorization metadata: %v", fields)
			}
			u, err := url.Parse(httpx.URL(out))
			if err != nil {
				t.Fatal(err)
			}
			if u.EscapedPath() != "/a%2Fb" || u.RawQuery != "q=a%2Bb" {
				t.Fatalf("URL encoding changed: %s", u)
			}
			lines := []string{}
			for _, name := range strings.Fields(fields["headers"]) {
				value := hdr.Get(name)
				switch name {
				case "(request-target)":
					value = strings.ToLower(verb) + " " + u.RequestURI()
				case "host":
					value = u.Host
				}
				lines = append(lines, name+": "+value)
			}
			if hdr.Get("X-Date") != "" {
				t.Fatal("stale x-date overrides signed date")
			}
			if body != nil {
				digest := sha256.Sum256(body)
				if hdr.Get("X-Content-Sha256") != base64.StdEncoding.EncodeToString(digest[:]) {
					t.Fatal("body digest does not cover the transmitted bytes")
				}
				if hdr.Get("Content-Length") != "16" {
					t.Fatalf("content length does not count UTF-8 bytes: %s", hdr.Get("Content-Length"))
				}
			}
			digest := sha256.Sum256([]byte(strings.Join(lines, "\n")))
			sig, err := base64.StdEncoding.DecodeString(fields["signature"])
			if err != nil {
				t.Fatal(err)
			}
			if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], sig); err != nil {
				t.Fatal(err)
			}
		})
	}
	empty, err := method.RequestTransform().Apply(httpx.NewRequestRecord("POST", "https://example.test/oci/buckets?", nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasSuffix(httpx.URL(empty), "?") || httpx.Header(empty).Get("Content-Length") != "0" {
		t.Fatal("empty request incorrectly signed")
	}
}

func TestOCICredentialCompositionAndConfigFile(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0600); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "config")
	if err := os.WriteFile(config, []byte("[DEFAULT]\ntenancy=t\nuser=u\nfingerprint=f\nkey_file=key.pem\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(AuthStruct{Type: string(KindOCI), ConfigFilePath: config}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OCI_TEST_MISSING", "")
	// Explicit raw credentials, including unresolved indirections, never fall
	// back to an otherwise valid config file with a different identity.
	for _, cfg := range []AuthStruct{
		{Type: string(KindOCI), TenancyOCID: "partial", ConfigFilePath: config},
		{Type: string(KindOCI), TenancyOCIDEnvVar: "OCI_TEST_MISSING", ConfigFilePath: config},
	} {
		if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "cannot compose OCI signing credentials") {
			t.Fatalf("partial raw configuration fell back: %v", err)
		}
	}
}
