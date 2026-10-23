package awsv4

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// AWS's published SigV4 test-suite "get-vanilla" case: a bare GET / on
// example.amazonaws.com, service "service", region us-east-1, at a fixed instant. The
// expected Authorization header is a fixed, well-known constant — matching it end-to-end
// proves canonical request → string-to-sign → signing key → signature are all correct.
func TestSignerGetVanilla(t *testing.T) {
	signer := NewSigV4Signer("us-east-1", "service", Credentials{
		AccessKeyID:     "AKIDEXAMPLE",
		SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
	}, false)
	req, err := http.NewRequest(http.MethodGet, "https://example.amazonaws.com/", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	when := time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)

	if err := signer.Sign(req, nil, when); err != nil {
		t.Fatalf("sign: %v", err)
	}

	const want = "AWS4-HMAC-SHA256 " +
		"Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, " +
		"SignedHeaders=host;x-amz-date, " +
		"Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"
	if got := req.Header.Get("Authorization"); got != want {
		t.Errorf("Authorization =\n  %s\nwant\n  %s", got, want)
	}
	if got := req.Header.Get("X-Amz-Date"); got != "20150830T123600Z" {
		t.Errorf("X-Amz-Date = %q, want 20150830T123600Z", got)
	}
}

// S3 signing must add and sign X-Amz-Content-Sha256; for an empty body that is the
// well-known hash of "". The signed-header list must therefore include it.
func TestSignerS3AddsContentHash(t *testing.T) {
	signer := NewSigV4Signer(
		"us-east-1", "s3", Credentials{AccessKeyID: "AKID", SecretAccessKey: "SECRET"}, true,
	)
	req, err := http.NewRequest(http.MethodGet, "https://s3.us-east-1.amazonaws.com/", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	if err := signer.Sign(req, nil, time.Unix(0, 0)); err != nil {
		t.Fatalf("sign: %v", err)
	}

	if got := req.Header.Get("X-Amz-Content-Sha256"); got != EmptyPayloadSHA256 {
		t.Errorf("X-Amz-Content-Sha256 = %q, want %q", got, EmptyPayloadSHA256)
	}
	if auth := req.Header.Get("Authorization"); !strings.Contains(auth, "SignedHeaders=") ||
		!strings.Contains(auth, "x-amz-content-sha256") {
		t.Errorf("Authorization does not sign x-amz-content-sha256: %s", auth)
	}
}

func TestSignerRejectsMissingCreds(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://example.com/", nil)
	if err := NewSigV4Signer("us-east-1", "s3", Credentials{}, false).Sign(req, nil, time.Unix(0, 0)); err == nil {
		t.Fatal("expected error signing with empty credentials, got nil")
	}
}

func TestSignerKeepsNegotiationHeadersUnsignedAndResigningStable(t *testing.T) {
	signer := NewSigV4Signer("ap-southeast-1", "logs", Credentials{AccessKeyID: "AKID", SecretAccessKey: "SECRET"}, false)
	request, err := http.NewRequest("POST", "https://logs.example.test/", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Accept-Encoding", "gzip")
	request.Header.Set("Content-Encoding", "amz-1.0")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Amz-Target", "Logs_20140328.GetLogEvents")
	when := time.Unix(1000, 0)
	if err = signer.Sign(request, []byte(`{}`), when); err != nil {
		t.Fatal(err)
	}
	first := request.Header.Get("Authorization")
	if !strings.Contains(first, "SignedHeaders=content-encoding;content-type;host;x-amz-date;x-amz-target,") {
		t.Fatalf("authorization=%s", first)
	}
	if request.Header.Get("Accept") != "application/json" {
		t.Fatal("dropped the wire negotiation header")
	}
	if err = signer.Sign(request, []byte(`{}`), when); err != nil {
		t.Fatal(err)
	}
	if got := request.Header.Get("Authorization"); got != first {
		t.Fatalf("re-signing changed authorization: %s != %s", got, first)
	}
}
