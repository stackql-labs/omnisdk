package authentication_test

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/stackql-labs/omnisdk/public/authentication"
)

func TestBearerVerifiesBeforeIdentity(t *testing.T) {
	hash := sha256.Sum256([]byte("secret"))
	a, err := authentication.NewBearer("local", []authentication.Token{{Subject: "alice", Digest: hash[:]}})
	if err != nil {
		t.Fatal(err)
	}
	hash[0] ^= 255 // construction must copy credential material
	for _, token := range []string{"", "wrong"} {
		if _, err := a.Authenticate(context.Background(), authentication.Evidence{Token: token}); err == nil {
			t.Fatal("invalid credential accepted")
		}
	}
	i, err := a.Authenticate(context.Background(), authentication.Evidence{Token: "secret"})
	if err != nil || i.Subject() != "alice" || i.Issuer() != "local" {
		t.Fatalf("identity=%v err=%v", i, err)
	}
}
func TestJWTConstraints(t *testing.T) {
	key := []byte("a-test-key-that-is-at-least-thirty-two-bytes-long")
	a, err := authentication.NewJWT("issuer", "omni", []string{"HS256"}, func(*jwt.Token) (any, error) { return key, nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, issuer, audience string
		exp                    time.Time
		key                    []byte
		valid                  bool
	}{
		{"valid", "issuer", "omni", time.Now().Add(time.Hour), key, true},
		{"issuer", "other", "omni", time.Now().Add(time.Hour), key, false},
		{"audience", "issuer", "other", time.Now().Add(time.Hour), key, false},
		{"expired", "issuer", "omni", time.Now().Add(-time.Hour), key, false},
		{"signature", "issuer", "omni", time.Now().Add(time.Hour), []byte("wrong-key"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims := jwt.RegisteredClaims{Issuer: tc.issuer, Subject: "alice", Audience: jwt.ClaimStrings{tc.audience}, ExpiresAt: jwt.NewNumericDate(tc.exp)}
			raw, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(tc.key)
			if err != nil {
				t.Fatal(err)
			}
			_, err = a.Authenticate(context.Background(), authentication.Evidence{Token: raw})
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
}
