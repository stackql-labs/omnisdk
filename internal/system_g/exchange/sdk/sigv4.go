// Package sdk holds provider building logic (AWS SigV4, GCP JWT) and thin factory functions
// that assemble provider exchanges from config, fed to the canonical httpx/plan/transform/bind
// constructors. It is a staging ground before the DSL that would otherwise generate them; no
// generic machinery lives here.
package sdk

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/httpx"
)

// Credentials is one AWS credential set. SessionToken is optional (STS/assumed-role).
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

// Signer signs an *http.Request in place before it is sent. Exchanges depend on this
// interface, not a concrete scheme, so SigV4 / bearer / no-op signers are interchangeable.
type Signer interface {
	// Sign adds the auth headers to req. payload is the exact body (nil = empty); now is
	// the signing instant, injected so signing is deterministic and testable.
	Sign(req *http.Request, payload []byte, now time.Time) error
}

var (
	_ Signer = sigV4Signer{}
)

// sigV4Signer implements AWS Signature Version 4 (header-based, AWS4-HMAC-SHA256) with no
// AWS SDK, so it can be verified offline against AWS's published test vectors.
//
// Scope/known limits (sufficient for the S3 listing exchanges here): the canonical path
// is single-encoded and not normalized (correct for S3 and for "/"), not double-encoded
// as the generic non-S3 rule requires; header values are trimmed but internal whitespace
// is not collapsed.
type sigV4Signer struct {
	region  string
	service string
	creds   Credentials
	// signPayloadHeader adds and signs X-Amz-Content-Sha256. S3 requires it; the generic
	// SigV4 flow (and its test vectors) omits it.
	signPayloadHeader bool
}

// NewSigV4Signer builds an AWS SigV4 signer. signPayloadHeader adds and signs
// X-Amz-Content-Sha256 (required by S3; omit for the generic SigV4 flow).
func NewSigV4Signer(region, service string, creds Credentials, signPayloadHeader bool) Signer {
	return sigV4Signer{
		region:            region,
		service:           service,
		creds:             creds,
		signPayloadHeader: signPayloadHeader,
	}
}

const (
	algorithm    = "AWS4-HMAC-SHA256"
	terminator   = "aws4_request"
	amzDateFmt   = "20060102T150405Z"
	dateStampFmt = "20060102"
	// EmptyPayloadSHA256 is hex(sha256("")) — the content hash for a bodyless request.
	EmptyPayloadSHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

// Sign adds X-Amz-Date, optionally X-Amz-Content-Sha256 and X-Amz-Security-Token, and the
// Authorization header to req. payload is the exact request body (nil = empty); now is the
// signing instant, injected so signing is deterministic and testable.
func (s sigV4Signer) Sign(req *http.Request, payload []byte, now time.Time) error {
	if s.creds.AccessKeyID == "" || s.creds.SecretAccessKey == "" {
		return errors.New("sdk: missing AWS credentials")
	}

	t := now.UTC()
	amzDate := t.Format(amzDateFmt)
	dateStamp := t.Format(dateStampFmt)
	payloadHash := hexSHA256(payload)

	req.Header.Set("X-Amz-Date", amzDate)
	if s.creds.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", s.creds.SessionToken)
	}
	if s.signPayloadHeader {
		req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	}

	canonicalHeaders, signedHeaders := s.canonicalHeaders(req)
	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI(req.URL),
		canonicalQuery(req.URL),
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	credentialScope := strings.Join([]string{dateStamp, s.region, s.service, terminator}, "/")
	stringToSign := strings.Join([]string{
		algorithm,
		amzDate,
		credentialScope,
		hexSHA256([]byte(canonicalRequest)),
	}, "\n")

	signature := hex.EncodeToString(hmacSHA256(s.signingKey(dateStamp), []byte(stringToSign)))
	req.Header.Set("Authorization", fmt.Sprintf(
		"%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		algorithm, s.creds.AccessKeyID, credentialScope, signedHeaders, signature,
	))
	return nil
}

// canonicalHeaders returns the sorted, newline-joined header block and the ";"-joined
// signed-header list. Host is signed from the URL (Go carries it off the header map).
func (s sigV4Signer) canonicalHeaders(req *http.Request) (block, signed string) {
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}

	type kv struct{ name, value string }
	hs := []kv{{"host", host}}
	for name, vals := range req.Header {
		hs = append(hs, kv{strings.ToLower(name), strings.TrimSpace(strings.Join(vals, ","))})
	}
	sort.Slice(hs, func(i, j int) bool { return hs[i].name < hs[j].name })

	var b strings.Builder
	names := make([]string, 0, len(hs))
	for _, h := range hs {
		b.WriteString(h.name)
		b.WriteByte(':')
		b.WriteString(h.value)
		b.WriteByte('\n')
		names = append(names, h.name)
	}
	return b.String(), strings.Join(names, ";")
}

func (s sigV4Signer) signingKey(dateStamp string) []byte {
	k := hmacSHA256([]byte("AWS4"+s.creds.SecretAccessKey), []byte(dateStamp))
	k = hmacSHA256(k, []byte(s.region))
	k = hmacSHA256(k, []byte(s.service))
	return hmacSHA256(k, []byte(terminator))
}

func canonicalURI(u *url.URL) string {
	if p := u.EscapedPath(); p != "" {
		return p
	}
	return "/"
}

func canonicalQuery(u *url.URL) string {
	q := u.Query()
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var parts []string
	for _, k := range keys {
		vals := append([]string(nil), q[k]...)
		sort.Strings(vals)
		for _, v := range vals {
			parts = append(parts, uriEncode(k)+"="+uriEncode(v))
		}
	}
	return strings.Join(parts, "&")
}

// uriEncode percent-encodes per RFC 3986, leaving only the unreserved set intact — the
// encoding AWS requires for canonical query components.
func uriEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func hexSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// sigV4Transform folds SigV4 auth headers into a request page, returning the re-signed request
// record. Pure page→page: it reads the request as a Page (method/url/headers/body) and returns a
// new request record — so it composes into httpx's request-transform chain (see httpx.Make).
type sigV4Transform struct {
	signer Signer
}

// NewSigV4Transform builds the signing transform.
func NewSigV4Transform(signer Signer) facade.Transform {
	return sigV4Transform{signer: signer}
}

func (t sigV4Transform) Apply(in facade.Page) (facade.Record, error) {
	method, u, body := httpx.Method(in), httpx.URL(in), httpx.ReqBody(in)
	req, err := http.NewRequest(method, u, nil)
	if err != nil {
		return nil, err
	}
	for name, vals := range httpx.Header(in) {
		for _, v := range vals {
			req.Header.Add(name, v)
		}
	}
	if err := t.signer.Sign(req, body, time.Now()); err != nil {
		return nil, err
	}
	// req.Header now carries the amz + Authorization headers; re-record it downstream.
	return httpx.NewRequestRecord(method, u, req.Header, body), nil
}
