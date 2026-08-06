package grpcx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/fullstorydev/grpcurl"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"

	"github.com/stackql-labs/omnisdk/internal/system_g/bind"
	"github.com/stackql-labs/omnisdk/internal/system_g/buffer"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/trace"
)

var _ facade.Operator = (*op)(nil)

// Continuation is page-token pagination — the only continuation gRPC list RPCs need. It mirrors
// httpx.ContPaginate: read NextTokenPath off each response, set PageTokenField on the next request.
type Continuation struct {
	PageTokenField string // request field carrying the next page token (e.g. "page_token")
	NextTokenPath  string // response JSON key holding the next token (e.g. "nextPageToken")
}

// Request declares a unary gRPC call. Like httpx.Request it is a finite, declarative description
// whose string values are {name} templates filled from the bound row; serde is descriptor-driven.
type Request struct {
	Target       string            // dial address (host:port)
	Method       string            // "pkg.Service.Method" or "pkg.Service/Method"
	Fields       map[string]any    // request message fields; string values are {name}-templated
	Metadata     map[string]string // outgoing metadata (e.g. "authorization": "Bearer {token}"), templated
	Continuation Continuation
}

// Make returns a bound-row → Operator factory (same shape as httpx.Make), so a gRPC call composes
// into the plan exactly like an HTTP one. dialOpts configure the connection (TLS/creds in prod; a
// bufconn dialer + insecure creds in tests). Each response is emitted as an agnostic doc record.
func Make(d *Descriptors, req Request, dialOpts ...grpc.DialOption) func(map[string]any) facade.Operator {
	return func(bound map[string]any) facade.Operator {
		return &op{d: d, req: req, bound: bound, dialOpts: dialOpts}
	}
}

type op struct {
	d        *Descriptors
	req      Request
	bound    map[string]any
	dialOpts []grpc.DialOption
}

func (o *op) Open(ctx context.Context) facade.Records {
	buf := buffer.NewBuffer(1, 1024, 0)
	go func() {
		var cerr error
		defer func() { buf.Complete(cerr) }()
		cerr = o.run(ctx, func(rec facade.Record) error { return buf.Append(ctx, rec) })
	}()
	return buf.Reader()
}

func (o *op) run(ctx context.Context, emit func(facade.Record) error) error {
	conn, err := grpc.NewClient(o.req.Target, o.dialOpts...)
	if err != nil {
		return fmt.Errorf("grpcx: dial %s: %w", o.req.Target, err)
	}
	defer conn.Close()

	method := strings.ReplaceAll(strings.TrimPrefix(o.req.Method, "/"), "/", ".")
	var headers []string
	for k, v := range o.req.Metadata {
		headers = append(headers, k+": "+subst(v, o.bound))
	}

	token := ""
	for {
		reqJSON, err := o.requestJSON(token)
		if err != nil {
			return err
		}
		rp, formatter, err := grpcurl.RequestParserAndFormatter(grpcurl.FormatJSON, o.d.src, strings.NewReader(reqJSON), grpcurl.FormatOptions{})
		if err != nil {
			return err
		}
		var out bytes.Buffer
		h := &grpcurl.DefaultEventHandler{Out: &out, Formatter: formatter}
		err = grpcurl.InvokeRPC(ctx, o.d.src, conn, method, headers, h, rp.Next)
		logWire(ctx, method, h.Status, err)
		if err != nil {
			return fmt.Errorf("grpcx: invoke %s: %w", method, err)
		}
		if h.Status != nil && h.Status.Err() != nil {
			return fmt.Errorf("grpcx: %s: %w", method, h.Status.Err())
		}

		doc := map[string]any{}
		if out.Len() > 0 {
			if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
				return fmt.Errorf("grpcx: decode response: %w", err)
			}
		}
		if err := emit(bind.NewDocRecord(doc)); err != nil {
			if errors.Is(err, buffer.ErrAllReadersClosed) {
				return nil
			}
			return err
		}
		if o.req.Continuation.NextTokenPath == "" {
			return nil
		}
		token = str(doc[o.req.Continuation.NextTokenPath])
		if token == "" {
			return nil
		}
	}
}

// requestJSON renders the request message as JSON (grpcurl's RequestParser turns it into the dynamic
// proto). String fields are {name}-templated from the bound row; the page token is injected here.
func (o *op) requestJSON(token string) (string, error) {
	m := make(map[string]any, len(o.req.Fields)+1)
	for k, v := range o.req.Fields {
		m[k] = substAny(v, o.bound)
	}
	if o.req.Continuation.PageTokenField != "" && token != "" {
		m[o.req.Continuation.PageTokenField] = token
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// logWire records one gRPC call to the run's trace sink, tagged "grpc" so the log distinguishes
// protocols at a glance (httpx logs the same lines tagged "http"). Method + outcome only — request
// messages and metadata (which carry the bearer token) are never logged.
func logWire(ctx context.Context, method string, st *status.Status, err error) {
	w := trace.Writer(ctx)
	if w == io.Discard {
		return
	}
	switch {
	case err != nil:
		fmt.Fprintf(w, "grpc %s → error: %v\n", method, err)
	case st != nil && st.Err() != nil:
		fmt.Fprintf(w, "grpc %s → %s: %s\n", method, st.Code(), st.Message())
	default:
		fmt.Fprintf(w, "grpc %s → OK\n", method)
	}
}

var reParam = regexp.MustCompile(`\{([a-zA-Z0-9_]+)\}`)

// subst replaces every {name} in s with the bound value (empty if absent), like httpx.subst.
func subst(s string, bound map[string]any) string {
	return reParam.ReplaceAllStringFunc(s, func(m string) string { return str(bound[m[1:len(m)-1]]) })
}

// substAny templates a string value; non-strings pass through unchanged (so ints stay JSON numbers).
func substAny(v any, bound map[string]any) any {
	if s, ok := v.(string); ok {
		return subst(s, bound)
	}
	return v
}

func str(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}
