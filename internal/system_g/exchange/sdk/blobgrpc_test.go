package sdk

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stackql-labs/omnisdk/internal/system_g/bind"
	"github.com/stackql-labs/omnisdk/internal/system_g/grpcx"
	"github.com/stackql-labs/omnisdk/internal/system_g/grpcx/grpctest"
	"github.com/stackql-labs/omnisdk/internal/system_g/plan"
)

// The gRPC bucket audit must yield the SAME normalized rows as the REST path — "equivalent queries,
// equivalent data" — using the SAME service-account auth (SA JWT → OAuth over HTTP → bearer on the
// gRPC call). Runs the real transport against the dynamic gRPC server + an OAuth stub.
func TestGCPBlobGRPCEquivalentRows(t *testing.T) {
	target, dialOpts, stop, err := grpctest.NewStorageServer([]grpctest.Bucket{
		{Name: "gcs-plain", PublicAccessPrevention: "inherited"},                                                                           // provider-managed, public, unversioned
		{Name: "gcs-cmek", KMSKey: "projects/p/locations/l/keyRings/r/cryptoKeys/k", PublicAccessPrevention: "enforced", Versioning: true}, // customer-managed, private, versioned
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"tok"}`))
	}))
	defer oauth.Close()

	d, err := grpcx.Load()
	if err != nil {
		t.Fatal(err)
	}

	rows := map[string]map[string]any{}
	rs := plan.ComposeRows(1, GCPBlobGRPCPlan(d, oauth.URL, target, testSACreds(t), "demo", dialOpts...)).Open(context.Background())
	defer rs.Close()
	for rs.Next(context.Background()) {
		m, _ := bind.DocMap(rs.Record())
		rows[m["name"].(string)] = m
	}
	if err := rs.Err(); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2: %v", len(rows), rows)
	}

	cmek := rows["gcs-cmek"]
	if cmek["provider"] != "gcp" || cmek["encryption_class"] != "customer-managed" || cmek["public"] != false || cmek["versioning"] != true {
		t.Errorf("gcs-cmek normalized wrong: %v", cmek)
	}
	plainB := rows["gcs-plain"]
	if plainB["encryption_class"] != "provider-managed" || plainB["public"] != true || plainB["versioning"] != false {
		t.Errorf("gcs-plain normalized wrong: %v", plainB)
	}
}

// testSACreds builds a valid service-account credential (generated RSA key) so the OAuth JWT signs.
func testSACreds(t *testing.T) GCPCredentials {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	saJSON, _ := json.Marshal(map[string]string{
		"client_email": "test@demo.iam.gserviceaccount.com",
		"private_key":  string(pemKey),
		"project_id":   "demo",
	})
	creds, err := ParseGCPCredentials(saJSON)
	if err != nil {
		t.Fatal(err)
	}
	return creds
}
