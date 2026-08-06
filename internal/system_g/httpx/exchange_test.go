package httpx

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

func drive(t *testing.T, op facade.Operator) []string {
	t.Helper()
	ctx := context.Background()
	rs := op.Open(ctx)
	defer rs.Close()
	var bodies []string
	for rs.Next(ctx) {
		v := rs.Record().Get(facade.AnonymousPayload)
		b, _ := io.ReadAll(v.Reader())
		bodies = append(bodies, string(b))
	}
	if err := rs.Err(); err != nil {
		t.Fatalf("stream err: %v", err)
	}
	return bodies
}

// Single request: {name} templates fill from the bound row, bearer auth is applied, and the
// response body is emitted.
func TestHTTPXSingleBearerSubst(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	req := Request{
		Method:  http.MethodGet,
		URL:     "{base}/things/{id}",
		Headers: map[string]string{"Authorization": "Bearer {token}"}, // bearer = templated header
	}
	bound := map[string]any{"base": srv.URL, "id": "42", "token": "secret-tok"}
	bodies := drive(t, Make(req, nil)(bound))

	if len(bodies) != 1 || bodies[0] != `{"ok":true}` {
		t.Fatalf("bodies = %v, want one {\"ok\":true}", bodies)
	}
	if gotAuth != "Bearer secret-tok" {
		t.Errorf("Authorization = %q, want Bearer secret-tok", gotAuth)
	}
	if gotPath != "/things/42" {
		t.Errorf("path = %q, want /things/42 (template not substituted)", gotPath)
	}
}

// A sign transform composes: httpx applies it to the built request record before sending, so
// its headers reach the wire. (This is exactly how SigV4 plugs in — no special-casing.)
func TestHTTPXSignTransformComposes(t *testing.T) {
	var gotSig string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSig = r.Header.Get("X-Signed")
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	req := Request{Method: http.MethodGet, URL: "{base}/x"}
	bodies := drive(t, Make(req, nil, signStub{})(map[string]any{"base": srv.URL}))

	if len(bodies) != 1 {
		t.Fatalf("want 1 response, got %d", len(bodies))
	}
	if gotSig != "yes" {
		t.Errorf("X-Signed = %q, want yes — sign transform not applied", gotSig)
	}
}

// Follow: paginate by chasing an absolute next-link URL from each response until it's absent.
func TestHTTPXFollowNextLink(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("p") == "2" {
			fmt.Fprint(w, `{"value":"b"}`)
			return
		}
		fmt.Fprintf(w, `{"value":"a","nextLink":"%s/x?p=2"}`, srv.URL)
	}))
	defer srv.Close()

	req := Request{
		Method:       http.MethodGet,
		URL:          "{base}/x",
		Continuation: Continuation{Kind: ContFollow, NextTokenPath: "nextLink"},
	}
	bodies := drive(t, Make(req, nil)(map[string]any{"base": srv.URL}))
	if len(bodies) != 2 {
		t.Fatalf("emitted %d pages, want 2 (follow both links)", len(bodies))
	}
	if JSONPath([]byte(bodies[0]), "value") != "a" || JSONPath([]byte(bodies[1]), "value") != "b" {
		t.Errorf("pages = %v, want [a, b]", bodies)
	}
}

// signStub is a stand-in sign transform: it adds a header to the request page, like SigV4.
type signStub struct{}

func (signStub) Apply(in facade.Page) (facade.Record, error) {
	h := Header(in)
	h.Set("X-Signed", "yes")
	return NewRequestRecord(Method(in), URL(in), h, ReqBody(in)), nil
}

// Poll: re-request until status reaches DONE, then emit that response.
func TestHTTPXPollUntilDone(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls < 3 {
			fmt.Fprint(w, `{"status":"RUNNING"}`)
			return
		}
		fmt.Fprint(w, `{"status":"DONE","targetId":"vpc-1"}`)
	}))
	defer srv.Close()

	req := Request{
		Method: http.MethodGet,
		URL:    "{op}",
		Continuation: Continuation{
			Kind: ContPoll, StatusPath: "status", DoneValue: "DONE",
			Interval: time.Millisecond, MaxAttempts: 10,
		},
	}
	bodies := drive(t, Make(req, nil)(map[string]any{"op": srv.URL}))

	if len(bodies) != 1 {
		t.Fatalf("emitted %d responses, want 1 (only the DONE one)", len(bodies))
	}
	if calls != 3 {
		t.Errorf("server hit %d times, want 3 (polled to DONE)", calls)
	}
	if JSONPath([]byte(bodies[0]), "targetId") != "vpc-1" {
		t.Errorf("final body = %q, want the DONE operation", bodies[0])
	}
}
