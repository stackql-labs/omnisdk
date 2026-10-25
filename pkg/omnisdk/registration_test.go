package omnisdk_test

import (
	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
	"gopkg.in/yaml.v3"
	"testing"
)

func TestProviderAuthPreservesDocumentIndirectionsAndFallback(t *testing.T) {
	var auth omnisdk.ProviderAuth
	document := `type: oci_signing_v1
tenancy_ocid_envvar: TENANCY
user_ocid_envvar: USER
oci_fingerprint_envvar: FINGERPRINT
oci_private_key_path_envvar: KEY_PATH
successor:
  type: api_key
  credentialsenvvar: FALLBACK
`
	if err := yaml.Unmarshal([]byte(document), &auth); err != nil {
		t.Fatal(err)
	}
	if auth.OciTenancyOCIDEnvVar != "TENANCY" || auth.OciUserOCIDEnvVar != "USER" || auth.OciFingerprintEnvVar != "FINGERPRINT" || auth.OciPrivateKeyFilePathEnvVar != "KEY_PATH" {
		t.Fatalf("lost document indirections: %#v", auth)
	}
	if auth.Successor == nil || auth.Successor.KeyEnvVar != "FALLBACK" {
		t.Fatal("lost successor credential declaration")
	}
}
