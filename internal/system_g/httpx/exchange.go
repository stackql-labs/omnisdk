package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/stackql-labs/omnisdk/internal/system_g/admit"
	"github.com/stackql-labs/omnisdk/internal/system_g/buffer"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/record"
	"github.com/stackql-labs/omnisdk/internal/system_g/retry"
	"github.com/stackql-labs/omnisdk/internal/system_g/trace"
	"github.com/stackql-labs/omnisdk/internal/system_g/value"
)

// KeyStatus is the response record key carrying the numeric HTTP status; the body is under
// facade.AnonymousPayload. The resolved request URL rides along under KeyURL (see request.go), so a
// failure (e.g. NewRequireOK on a 403) can name the site instead of leaving the caller to guess.
const KeyStatus = "status"

// Make returns the InnerFactory (bound → operator) for a Request. The generic exchange
// substitutes {name} templates from the bound row, runs the request-path transform chain over the
// built request record, sends, and drives the configured continuation, emitting one response
// record per response. reqT is the request pipeline — zero or more facade.Transforms over the
// request record (e.g. SigV4), each built with ReqEdge and applied in order. Bearer auth needs no
// transform — it is a templated header.
// decode turns a raw response body into the agnostic document that continuation paths are read
// against (see docValue). It is a drop-in transform: mxj/XML today (transform.NewXMLToAgnostic),
// swappable when its lossiness bites, or JSON — the encoding is not welded into httpx. nil means
// "JSON by default" (so JSON callers pass nothing), which is the only encoding httpx knows on its
// own; anything else is injected. reqT is the request pipeline (e.g. SigV4).
func Make(req Request, decode facade.Transform, reqT ...facade.Transform) func(bound map[string]any) facade.Operator {
	return func(bound map[string]any) facade.Operator {
		return &op{req: req, bound: bound, decode: decode, reqT: reqT, client: http.DefaultClient}
	}
}

type op struct {
	req    Request
	bound  map[string]any
	decode facade.Transform
	reqT   []facade.Transform
	client *http.Client
}

func (o *op) Open(ctx context.Context) facade.Records {
	buf := buffer.NewBuffer(1, 1024, 0)
	go func() {
		var cerr error
		defer func() { buf.Complete(cerr) }()
		switch o.req.Continuation.Kind {
		case ContPoll:
			cerr = o.poll(ctx, buf)
		case ContPaginate:
			cerr = o.paginate(ctx, buf)
		case ContFollow:
			cerr = o.follow(ctx, buf)
		default:
			cerr = o.once(ctx, buf, "")
		}
	}()
	return buf.Reader()
}

func (o *op) once(ctx context.Context, buf facade.Buffer, token string) error {
	status, body, reqURL, err := o.do(ctx, token, "")
	if err != nil {
		return err
	}
	return emit(ctx, buf, status, body, reqURL)
}

func (o *op) poll(ctx context.Context, buf facade.Buffer) error {
	c := o.req.Continuation
	for i := 0; i < c.MaxAttempts; i++ {
		status, body, reqURL, err := o.do(ctx, "", "")
		if err != nil {
			return err
		}
		if o.docValue(body, c.StatusPath) == c.DoneValue {
			return emit(ctx, buf, status, body, reqURL)
		}
		select {
		case <-time.After(c.Interval):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return fmt.Errorf("httpx: poll of %s did not reach %s=%s after %d attempts",
		subst(o.req.URL, o.bound), c.StatusPath, c.DoneValue, c.MaxAttempts)
}

func (o *op) paginate(ctx context.Context, buf facade.Buffer) error {
	token := ""
	for {
		status, body, reqURL, err := o.do(ctx, token, "")
		if err != nil {
			return err
		}
		if err := emit(ctx, buf, status, body, reqURL); err != nil {
			return err
		}
		next := o.docValue(body, o.req.Continuation.NextTokenPath)
		if next == "" {
			return nil
		}
		token = next
	}
}

// follow paginates by following an absolute next-link URL from each response (Azure nextLink,
// GCP nextPageToken-as-URL), rather than a token query param. NextTokenPath yields the link.
func (o *op) follow(ctx context.Context, buf facade.Buffer) error {
	next := ""
	for {
		status, body, reqURL, err := o.do(ctx, "", next)
		if err != nil {
			return err
		}
		if err := emit(ctx, buf, status, body, reqURL); err != nil {
			return err
		}
		next = o.docValue(body, o.req.Continuation.NextTokenPath)
		if next == "" {
			return nil
		}
	}
}

// do builds a request record and sends it through the request-path transform chain. token (if
// set) is added as the paginate token param; overrideURL (if set) is used verbatim as the request
// URL, skipping templating and query assembly — for follow's absolute next-links.
func (o *op) do(ctx context.Context, token, overrideURL string) (int, []byte, string, error) {
	body, contentType := buildBody(o.req.Body, o.bound)

	u := overrideURL
	if u == "" {
		u = subst(o.req.URL, o.bound)
		q := url.Values{}
		for k, v := range o.req.Query {
			q.Set(subst(k, o.bound), subst(v, o.bound))
		}
		if token != "" && o.req.Continuation.TokenParam != "" {
			q.Set(o.req.Continuation.TokenParam, token)
		}
		if len(q) > 0 {
			u += "?" + q.Encode()
		}
	}

	header := http.Header{}
	for k, v := range o.req.Headers {
		header.Set(subst(k, o.bound), subst(v, o.bound))
	}
	if contentType != "" {
		header.Set("Content-Type", contentType)
	}

	var rec facade.Record = NewRequestRecord(subst(o.req.Method, o.bound), u, header, body)
	for _, t := range o.reqT {
		if t == nil {
			continue
		}
		next, err := t.Apply(rec) // rec is a Record, read by the transform as a Page
		if err != nil {
			return 0, nil, u, err
		}
		if next != nil {
			rec = next
		}
	}
	status, respBody, err := o.send(ctx, rec)
	return status, respBody, u, err
}

// send performs a (possibly signed) request record, recovering from ephemeral failures per the
// run's shared RetryPolicy (retry.From(ctx); a no-op policy = no retry). A 2xx returns at once; a
// transport error or a >=400 status is handed to the policy, which decides whether to reattempt
// and staggers the wait. A status the policy declines to retry (e.g. 404) is returned to the
// caller unchanged, so downstream branching still sees it.
func (o *op) send(ctx context.Context, rec facade.Record) (int, []byte, error) {
	// Admission: hold one concurrency slot for this backend (keyed by host) across all attempts,
	// so concurrent requests to the same account/service are bounded. No admissions set = open.
	tok, err := admit.From(ctx).For(scopeOf(URL(rec))).Acquire(ctx)
	if err != nil {
		return 0, nil, err
	}
	defer tok.Release()

	pol := retry.From(ctx)
	start := time.Now()
	var prev time.Duration
	for i := 0; ; i++ {
		status, body, header, err := o.sendOnce(ctx, rec)
		logWire(ctx, Method(rec), URL(rec), status, err, body)
		if err == nil && status < 400 {
			return status, body, nil
		}
		wait, ok := pol.Recover(ctx, facade.Attempt{
			Index: i, Status: status, Err: err,
			RetryAfter: retryAfter(header), Elapsed: time.Since(start), PrevWait: prev,
		})
		if !ok {
			return status, body, err // give up: permanent, or no retry policy
		}
		prev = wait
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return 0, nil, ctx.Err()
		}
	}
}

// logWire records one attempt to the run's trace sink (io.Discard = silent): the request line and
// its outcome. Failures carry the response body, so a 403 is visible in the traffic log — not just
// on stderr as a bare error. Request bodies and headers are never logged (they carry the bearer /
// jwt-bearer assertion); URLs carry no secrets in this sdk (auth rides headers).
func logWire(ctx context.Context, method, rawURL string, status int, err error, body []byte) {
	w := trace.Writer(ctx)
	if w == io.Discard {
		return
	}
	switch {
	case err != nil:
		fmt.Fprintf(w, "http %s %s → transport error: %v\n", method, rawURL, err)
	case status >= 400:
		fmt.Fprintf(w, "http %s %s → %d %s\n", method, rawURL, status, string(body))
	default:
		fmt.Fprintf(w, "http %s %s → %d\n", method, rawURL, status)
	}
}

// sendOnce performs exactly one request and returns the status, body, and response header.
func (o *op) sendOnce(ctx context.Context, rec facade.Record) (int, []byte, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, Method(rec), URL(rec), bytes.NewReader(ReqBody(rec)))
	if err != nil {
		return 0, nil, nil, err
	}
	for k, vs := range Header(rec) {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := o.client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, resp.Header, err
	}
	return resp.StatusCode, payload, resp.Header, nil
}

// scopeOf is the admission scope key for a request URL: its host (scheme+host), a provider-
// agnostic proxy for "the same backend" — all calls to one service endpoint share a limiter. The
// sdk can key more precisely (by account) later; host is the correct default here.
func scopeOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return rawURL
	}
	return u.Scheme + "://" + u.Host
}

// retryAfter parses a Retry-After header (delta-seconds or HTTP-date) into a duration (0 if
// absent/unparseable). It is the server's own backpressure signal.
func retryAfter(h http.Header) time.Duration {
	if h == nil {
		return 0
	}
	v := h.Get("Retry-After")
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

func buildBody(b Body, bound map[string]any) (body []byte, contentType string) {
	switch b.Encoding {
	case EncodingForm:
		vals := url.Values{}
		for k, v := range b.Params {
			vals.Set(k, substAny(v, bound))
		}
		return []byte(vals.Encode()), "application/x-www-form-urlencoded"
	case EncodingJSON:
		m := make(map[string]any, len(b.Params))
		for k, v := range b.Params {
			if s, ok := v.(string); ok {
				m[k] = subst(s, bound)
			} else {
				m[k] = v
			}
		}
		bs, _ := json.Marshal(m)
		return bs, "application/json"
	default:
		return nil, ""
	}
}

var reParam = regexp.MustCompile(`\{([a-zA-Z0-9_]+)\}`)

// subst replaces every {name} in s with the bound value (empty if absent).
func subst(s string, bound map[string]any) string {
	return reParam.ReplaceAllStringFunc(s, func(m string) string {
		return str(bound[m[1:len(m)-1]])
	})
}

func substAny(v any, bound map[string]any) string {
	if s, ok := v.(string); ok {
		return subst(s, bound)
	}
	return str(v)
}

// docValue reads a continuation path off the decoded response. The decoder is swappable: nil =
// JSON (the only encoding httpx knows unaided); otherwise the injected decode transform turns the
// raw body into an agnostic document (mxj/XML, or anything else) and the path is read off that.
// So JSON and XML differ only in the decoder — no encoding is welded into the continuation logic.
func (o *op) docValue(raw []byte, path string) string {
	if path == "" {
		return ""
	}
	if o.decode == nil {
		return JSONPath(raw, path)
	}
	rec, err := o.decode.Apply(record.NewRecord(map[string]facade.Value{
		facade.AnonymousPayload: value.NewBytesValue(raw),
	}))
	if err != nil || rec == nil {
		return ""
	}
	doc, ok := rec.Doc(facade.AnonymousPayload)
	if !ok {
		return ""
	}
	return DocPath(doc, path)
}

// DocPath returns the string at a dotted path in an agnostic document tree ("" if
// missing/non-scalar). Encoding-agnostic: works on whatever a decode transform produced. A
// segment landing on a list (e.g. mxj's repeated elements) descends its first element, so a
// single-occurrence XML element reads the same whether the decoder yields a map or a 1-list.
func DocPath(doc any, path string) string {
	m := doc
	for _, seg := range strings.Split(path, ".") {
		if arr, ok := m.([]any); ok {
			if len(arr) == 0 {
				return ""
			}
			m = arr[0]
		}
		obj, ok := m.(map[string]any)
		if !ok {
			return ""
		}
		m = obj[seg]
	}
	if arr, ok := m.([]any); ok {
		if len(arr) == 0 {
			return ""
		}
		m = arr[0]
	}
	return str(m)
}

// JSONPath returns the string at a dotted path in a JSON body ("" if missing/non-scalar). It is
// DocPath over a JSON decode — the built-in default decoder.
func JSONPath(body []byte, path string) string {
	if path == "" {
		return ""
	}
	var m any
	if err := json.Unmarshal(body, &m); err != nil {
		return ""
	}
	return DocPath(m, path)
}

func emit(ctx context.Context, buf facade.Buffer, status int, body []byte, reqURL string) error {
	rec := record.NewRecord(map[string]facade.Value{
		KeyStatus:               value.NewBytesValue([]byte(strconv.Itoa(status))),
		KeyURL:                  value.NewBytesValue([]byte(reqURL)),
		facade.AnonymousPayload: value.NewBytesValue(body),
	})
	if err := buf.Append(ctx, rec); err != nil {
		if errors.Is(err, buffer.ErrAllReadersClosed) {
			return nil
		}
		return err
	}
	return nil
}

func str(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", t)
	}
}
