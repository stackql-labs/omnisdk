package auth_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/stackql-labs/omnisdk/internal/system_g/auth"
)

func selfSigned(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "u"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
}

// A kubeconfig user with a client certificate authenticates by presenting it, with no header.
func TestKubeconfigClientCertificate(t *testing.T) {
	cert, key := selfSigned(t)
	b64 := base64.StdEncoding.EncodeToString
	kc := "clusters:\n- name: c\n  cluster: {certificate-authority-data: " + b64(cert) + "}\n" +
		"users:\n- name: u\n  user:\n    client-certificate-data: " + b64(cert) + "\n    client-key-data: " + b64(key) + "\n" +
		"contexts:\n- name: ctx\n  context: {cluster: c, user: u}\n"
	m, err := auth.New(auth.AuthStruct{Type: "kubeconfig", Credentials: kc, Context: "ctx"})
	if err != nil {
		t.Fatal(err)
	}
	tc, ok := m.(auth.TLSConfigurer)
	if !ok || len(tc.TLSConfig().Certificates) != 1 || tc.TLSConfig().RootCAs == nil {
		t.Fatalf("TLS = %+v, want the client certificate and the cluster CA", tc)
	}
	if m.RequestTransform() != nil {
		t.Error("a certificate user sent a header too")
	}
}

// A successor's header follows its predecessor's; a token exchange cannot be chained.
func TestSuccessorChains(t *testing.T) {
	if _, err := auth.New(auth.AuthStruct{Type: "custom", Name: "A", Credentials: "a",
		Successor: &auth.AuthStruct{Type: "custom", Name: "B", Credentials: "b"}}); err != nil {
		t.Fatalf("chain: %v", err)
	}
	if _, err := auth.New(auth.AuthStruct{Type: "custom", Name: "A", Credentials: "a",
		Successor: &auth.AuthStruct{Type: "client_credentials", TokenURL: "https://x/token"}}); err == nil {
		t.Error("chained a token exchange")
	}
}
