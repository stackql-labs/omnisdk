// Package httpx is the generic, config-driven HTTP exchange. Every HTTP exchange is a data
// instance of a finite vocabulary (Request/Body/Continuation) — not bespoke code per exchange.
// String fields are templates: {name} is substituted from the bound input row. Auth is not part
// of the config: it composes as a sign transform (bearer is just a templated header).
package httpx

import "time"

// Encoding of a request body.
type Encoding string

const (
	EncodingNone Encoding = "none"
	EncodingForm Encoding = "form" // application/x-www-form-urlencoded
	EncodingJSON Encoding = "json" // application/json
)

// ContinuationKind selects how multi-response calls are driven.
type ContinuationKind string

const (
	ContNone     ContinuationKind = "none"
	ContPaginate ContinuationKind = "paginate" // re-request with a next-token query param until absent
	ContPoll     ContinuationKind = "poll"     // re-request until a status field reaches DoneValue
	ContFollow   ContinuationKind = "follow"   // follow an absolute next-link URL until absent (Azure nextLink)
)

// Body is the request body: an encoding and templated params (string values are substituted;
// non-string values pass through, so JSON bodies can carry bools/numbers).
type Body struct {
	Encoding Encoding
	Params   map[string]any
}

// Continuation drives multi-response calls. Paths are dotted paths into the decoded response.
type Continuation struct {
	Kind ContinuationKind

	// paginate: read NextTokenPath from each response; if present, re-request sending it as
	// query/body param TokenParam, until absent.
	NextTokenPath string
	TokenParam    string

	// poll: re-request (same URL) until the value at StatusPath equals DoneValue.
	StatusPath  string
	DoneValue   string
	Interval    time.Duration
	MaxAttempts int
}

// Request fully describes one HTTP exchange as data. Every string is a {name} template.
type Request struct {
	Method       string
	URL          string
	Query        map[string]string
	Headers      map[string]string
	Body         Body
	Continuation Continuation
}
