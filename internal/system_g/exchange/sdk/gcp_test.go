package sdk

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"
	"time"
)

// The OAuth exchange's assertion must be a well-formed, correctly-signed RS256 JWT with the
// jwt-bearer claims. Generate a key, round-trip it through ParseGCPCredentials, sign, then
// verify structure + signature with the public key.
func TestGCPSignedJWT(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	saJSON, _ := json.Marshal(map[string]string{
		"client_email": "svc@proj.iam.gserviceaccount.com",
		"private_key":  string(keyPEM),
		"token_uri":    "https://oauth2.googleapis.com/token",
		"project_id":   "proj",
	})

	creds, err := ParseGCPCredentials(saJSON)
	if err != nil {
		t.Fatalf("parse creds: %v", err)
	}
	jwt, err := creds.signedJWT("https://oauth2.googleapis.com/token", gcpComputeScope, time.Unix(1000, 0))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt has %d parts, want 3", len(parts))
	}

	// signature verifies against the public key over header.claims
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		t.Errorf("signature does not verify: %v", err)
	}

	// claims carry the jwt-bearer fields
	var claims struct {
		Iss, Scope, Aud string
		Iat, Exp        int64
	}
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatalf("claims json: %v", err)
	}
	if claims.Iss != "svc@proj.iam.gserviceaccount.com" || claims.Scope != gcpComputeScope ||
		claims.Aud != "https://oauth2.googleapis.com/token" || claims.Exp <= claims.Iat {
		t.Errorf("claims = %+v", claims)
	}
}
