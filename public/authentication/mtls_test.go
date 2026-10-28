package authentication_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/stackql-labs/omnisdk/public/authentication"
)

func TestMTLSVerifiesBeforeMapping(t *testing.T) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "root"}, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	root, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(root)
	mapped := 0
	a, err := authentication.NewMTLS("certs", roots, func(c *x509.Certificate) (string, error) {
		mapped++
		if len(c.DNSNames) != 1 {
			return "", errors.New("missing identity SAN")
		}
		return c.DNSNames[0], nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, expired := range []bool{false, true} {
		cert := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: []string{"alice.example"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
		if expired {
			cert.NotAfter = time.Now().Add(-time.Minute)
		}
		der, err := x509.CreateCertificate(rand.Reader, cert, root, pub, key)
		if err != nil {
			t.Fatal(err)
		}
		leaf, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		i, err := a.Authenticate(context.Background(), authentication.Evidence{TLS: &tls.ConnectionState{HandshakeComplete: true, PeerCertificates: []*x509.Certificate{leaf}}})
		if expired {
			if err == nil || mapped != 1 {
				t.Fatal("unverified certificate mapped")
			}
		} else if err != nil || i.Subject() != "alice.example" {
			t.Fatalf("identity=%v err=%v", i, err)
		}
	}
	if _, err := a.Authenticate(context.Background(), authentication.Evidence{}); err == nil {
		t.Fatal("missing certificate accepted")
	}
}
func TestLocalOSRejectsRemoteEvidence(t *testing.T) {
	a, err := authentication.NewLocalOS("host-one")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Authenticate(context.Background(), authentication.Evidence{Token: "root"}); err == nil {
		t.Fatal("remote username trusted")
	}
	if _, err := a.Authenticate(context.Background(), authentication.Evidence{}); err != nil {
		t.Fatal(err)
	}
}
