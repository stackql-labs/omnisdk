package facade

import (
	"context"
	"io"
	"time"
)

var (
// defaultOutStream = os.Stderr
// defaultWriteBuffer =
)

const (
	AnonymousPayload = "_anon"
)

type systemG struct {
	writer io.Writer
}

// Page is the materialized view of a tuple that a Transform reads: every value is in hand —
// raw bytes or an agnostic document — with no Reader, cursor, operator, or source behind it. It
// is the entire surface a transform sees, which is what makes the Transform invariant hold by
// construction: a transform cannot block, pull, or reach a source, because a Page exposes none.
type Page interface {
	// Bytes returns the materialized bytes at key k (nil if absent).
	Bytes(k string) []byte
	// Doc returns the agnostic document at key k (false if absent or not a document).
	Doc(k string) (any, bool)
}

// Record is one tuple flowing along an edge, addressed by designation (key). Read-only: values
// are fixed at construction, so a published Record is immutable by type, not by convention.
// Concrete impl lives in the record package. A Record is also a Page — operators get the full
// value surface (Get → Value → Reader) for streaming; transforms get only the Page view.
type Record interface {
	Page
	// Get returns the value for key k (nil if absent).
	Get(k string) Value
	// Len is the number of slots.
	Len() int
}

// Records is a pull cursor over a stream of records (Volcano-style). Consumers drive
// it; nothing is produced until Next is called, and Close releases the consumer's hold.
type Records interface {
	// Next advances to the next record, blocking at the tail until one is published or
	// the producer completes. Returns false at EOF, on error, or on ctx cancellation.
	Next(ctx context.Context) bool
	// Record returns the current record; valid only after Next returned true.
	Record() Record
	// Err returns the terminal error, checked after Next returns false.
	Err() error
	// Close releases this cursor's refs so consumed records can be reclaimed.
	Close() error
}

// Encoder renders a record to bytes on the egress path (presentation layer). Kept off
// Record so presentation is a swappable strategy, not welded into the data.
type Encoder interface {
	Encode(w io.Writer, r Record) (int64, error)
}

type Decoder interface {
	Decode(chunk []byte) (Record, error)
}

// Transform is a page-to-page function: given one materialized input Page, it returns one
// Record. The invariant is enforced by the signature, not left to discipline:
//
//   - source-oblivious: its only input is a Page, which exposes no operator, cursor, or source.
//     It cannot tell what produced its input, so it has no recourse to reach back to it.
//   - no eager pull: a Page is already materialized; there is nothing to pull and no way to ask.
//   - non-blocking: no ctx, no Reader, no I/O handle — it computes from the Page and returns.
//
// So transforms compose as plain function composition over a page — a chain is nested Apply
// calls, with nothing to Open or drain. Anything that must drain a stream, await, or read the
// per-run context (cancellation, the trace writer) is an Operator (Open → Records), never a
// Transform. The returned Record is the transform's own construction; downstream reads it as a
// Page in turn.
type Transform interface {
	Apply(in Page) (Record, error)
}

// Progress is the loop state a TerminationPolicy inspects.
type Progress struct {
	Round   int // rounds completed so far
	Emitted int // records emitted so far
}

// TerminationPolicy is a well-founded backstop for otherwise-unbounded loops (SCC
// fixpoint iteration, pagination, cycles). Stop is consulted before each round; a true
// result ends the loop. It is independent of fixpoint detection — that stays with the
// operator. Concrete policies live in the termination package.
type TerminationPolicy interface {
	Stop(p Progress) bool
}

// Attempt is the outcome of one try of a potentially-ephemeral operation, handed to a
// RetryPolicy to decide the next move. A read-only value (DTO), not a behaviour.
type Attempt struct {
	Index      int           // 0-based try number (0 = the first, failed, try)
	Status     int           // HTTP status; 0 if the request never completed
	Err        error         // transport/protocol error; nil on a status-only failure
	RetryAfter time.Duration // server backpressure hint (Retry-After); 0 if none
	Elapsed    time.Duration // wall time since the first try began
	PrevWait   time.Duration // wait applied before this attempt (for stateful backoff, e.g. decorrelated jitter)
}

// RetryPolicy governs recovery from potentially-ephemeral failures. A single RetryPolicy is
// shared by every concurrent request in a run, so implementations MUST be safe for concurrent
// use and SHOULD govern AGGREGATE retry load — under a dependency outage, independent per-call
// retries amplify it into a storm, so a shared budget/rate/circuit belongs behind this interface,
// not in the caller. The returned wait is staggered (jittered) so simultaneous failures
// de-correlate. Concrete policies live in the retry package; carried per-run on the context.
type RetryPolicy interface {
	// Recover reports whether to reattempt after a failed Attempt, and how long to wait first.
	// ok=false gives up — permanent failure, budget exhausted, circuit open, or backstop hit.
	Recover(ctx context.Context, a Attempt) (wait time.Duration, ok bool)
}

// Token is a held admission slot. Release returns it to its Limiter; call it exactly once when
// the guarded work completes (idiomatically via defer).
type Token interface {
	Release()
}

// Limiter bounds concurrent in-flight work sharing one scope. Acquire blocks (honouring ctx) for
// a slot and returns a Token to release when the work finishes. Shared across callers; safe for
// concurrent use. A fixed impl is a semaphore of N ("N at a time"); an adaptive impl resizes N
// under degradation. It is the tunable answer to "how optimistic to be with concurrency".
type Limiter interface {
	Acquire(ctx context.Context) (Token, error)
}

// Admissions hands out the Limiter governing a scope key, so all work sharing a backend (e.g. one
// cloud account/region/service) contends on one Limiter while unrelated scopes run free. This
// keying is the point: independent nodes across independent graphs still throttle together when,
// and only when, they hit the same resource. Safe for concurrent use.
type Admissions interface {
	For(key string) Limiter
}

// SagaEntry is one logged step of a saga: the forward (Redo) action a mutating exchange performed
// and its compensating (Undo) action, with the keys identifying the affected resource. It records
// intent only — replay/rollback execution is future work. A read-only value (DTO).
type SagaEntry struct {
	Exchange string            // the exchange that committed
	Redo     string            // forward action to replay it, e.g. "CreateVpc"
	Undo     string            // compensating action to reverse it, e.g. "DeleteVpc"
	Keys     map[string]string // identifiers for both (e.g. {"vpc_id": "vpc-123"})
}

// SagaLog records saga entries for later compensation/replay. Written to as mutating exchanges
// commit; safe for concurrent use. Execution of the recorded undo/redo is deliberately not part
// of this contract yet — this only durably captures what would be undone or redone.
type SagaLog interface {
	Record(e SagaEntry)
}

// EdgeID is the AOT-assigned handle for one of a node's incident β edges.
type EdgeID string

// Buffer is a single-writer, multi-reader append-only store sitting on a β edge:
// the producer appends once, each consumer pulls via its own Records cursor.
type Buffer interface {
	// Append publishes one immutable record. Blocks under back-pressure until a slot
	// frees or ctx is done.
	Append(ctx context.Context, r Record) error
	// Complete marks EOF; err may be nil. No Append may follow.
	Complete(err error)
	// Reader returns a fresh pull cursor over all records. Its Close releases this
	// reader's hold; when every reader has passed a record it is reclaimed (low-water).
	Reader() Records
}

func (g *systemG) Print() {
	g.writer.Write([]byte("system g"))
}

type Attribute interface {
	Node
	GetType() Type
	GetValue() (any, bool)
}

type Type interface {
	Component
	Name() string
	Equals(other Type) bool
}

type Value interface {
	Component
	Type() Type
	Reader() io.Reader // pull, not push — consumer sets the pace
}

// Component is the single printable primitive: every graph component can write itself
// to a writer. It is exactly io.WriterTo, so io.Copy and friends work for free.
type Component interface {
	io.WriterTo
}

// Node is a printable graph vertex.
type Node interface {
	Component
}

// Bindable participates in β: it receives inbound bindings and emits outbound ones.
type Bindable interface {
	Receives() []Beta // inbound β edges
	Emits() []Beta    // outbound β edges
}

type Wirable interface { // build view
	AddEmit(Beta)
	AddReceive(Beta)
}

// Operator is a Volcano pull node: Open returns a cursor; ranging it drives execution
// down the tree. The driver kicks off the plan by opening the root.
type Operator interface {
	Open(ctx context.Context) Records
}

// Exchange is an exchange vertex — a state machine over states — wired by β.
type Exchange interface {
	Node
	Bindable
	Wirable
	Operator
}

// SCC is a condensed strongly-connected-component vertex (Tarjan condensation, §W),
// wired by its boundary β edges.
type SCC interface {
	Node
	Bindable
	Operator
}

type Beta interface {
	Component
	Publish(io.Writer)
}

// Alpha is a behavioural (control-flow) edge (E_α, §E_α). Unlike β it carries no data — it
// carries behaviour: annotations that govern how the edge is traversed. Timing is the first
// such annotation (Σ events / Signal come later). Concrete impls live outside facade.
type Alpha interface {
	Component
	// Delay is the timing annotation: traversal of this edge waits this long (0 = none).
	Delay() time.Duration
}

// FormClass is the system-wide class of side effect an exchange enacts (§saga, F).
type FormClass int

const (
	FormRead FormClass = iota // no side effect
	FormCreate
	FormUpdate
	FormDelete
)

// Inverse is the compensating form ι(f): the action that undoes f on rollback.
// ok is false where no compensation exists (⊥) — flags a non-reversible plan AOT.
func (f FormClass) Inverse() (inv FormClass, ok bool) {
	switch f {
	case FormRead:
		return FormRead, true // no-op inverse
	case FormCreate:
		return FormDelete, true
	case FormUpdate:
		return FormUpdate, true // restore prior state
	case FormDelete:
		return FormDelete, false // ⊥: cannot un-delete
	default:
		return f, false
	}
}
