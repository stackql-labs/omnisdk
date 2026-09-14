package facade

import (
	"context"
	"errors"
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
	// Name is what the document calls it — "string", "integer", a provider's own name. Open-ended,
	// provider-specific, and carries no behaviour.
	Name() string
	// Format is the document's refinement of Name: int64, double, date-time, cidr. Also data, also
	// open-ended; two formats sharing a Kind behave identically and differ only in what they mean.
	Format() string
	// Kind is what can be done with values of this type: parse, compare, encode. Many types share
	// one kind — int32, int64 and unsignedLong are all integers — which is why behaviour hangs here
	// and not on Name.
	Kind() Kind
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

// LedgerKey names one managed resource. Keyed by *name*, never by content: key-by-content makes
// every edit a new resource, and key-by-name without stored content makes every edit invisible.
// Prefixed by scope, so a run lists and reads only its own subtree.
type LedgerKey string

// LedgerPhase is where an entry sits between intent and committed fact.
type LedgerPhase int

const (
	// LedgerPending — intent recorded, outcome unknown. Proposed is valid, Identity is not.
	LedgerPending LedgerPhase = iota
	// LedgerLive — applied. Plan and Identity are valid, Proposed is not.
	LedgerLive
)

// LedgerVersion is an opaque compare-and-swap token: an ETag on object storage, a filename
// version on local disk. Held from a read and presented on the matching write, so a concurrent
// writer is detected rather than silently overwritten.
type LedgerVersion string

// LedgerVersionNone asserts the entry does not exist. Presented to Begin it means create-only,
// the analogue of If-None-Match:* and O_EXCL.
const LedgerVersionNone LedgerVersion = ""

// LedgerEntry is the durable record for one key. Two content slots, not one: Begin must never
// overwrite Plan, or a failed call loses n-1 and with it the ability to unset a dropped field.
// Proposed is promoted to Plan at Resolve. A read-only value.
type LedgerEntry interface {
	Key() LedgerKey
	Phase() LedgerPhase
	// Plan is n-1: the last resolved intent, and the basis for a three-way merge.
	Plan() []byte
	// Proposed is n: the intent of the run in flight. Valid only while LedgerPending.
	Proposed() []byte
	// Identity is everything the inverse call needs to address the object — not merely an id,
	// since a compensation is derived from Plan and Identity alone.
	Identity() []byte
}

// Ledger is the durable, per-key write-ahead log of resolved intent. Redo-only: a pending entry
// says what to go and ask about, never what to blindly re-execute. It records intent, never API
// responses — actual state is read live, and only identity is persisted from it.
//
// Every mutation is compare-and-swap against the version from a prior Get, so there is no lock on
// the entry path and readers never block. Safe for concurrent use.
type Ledger interface {
	// Get returns the entry and the version to present on a subsequent write.
	Get(ctx context.Context, k LedgerKey) (LedgerEntry, LedgerVersion, bool, error)
	// List returns every entry whose key falls under scope. Pruning depends on it.
	List(ctx context.Context, scope string) ([]LedgerEntry, error)
	// Begin records intent before the wire effect: phase becomes LedgerPending and proposed is
	// stored, leaving Plan untouched. Pass LedgerVersionNone to require the key be absent.
	// Repeating Begin with an identical proposal is a no-op, so a retried run does not conflict
	// with itself.
	Begin(ctx context.Context, k LedgerKey, proposed []byte, v LedgerVersion) error
	// Resolve commits: Proposed is promoted to Plan, identity is stored, phase becomes LedgerLive.
	Resolve(ctx context.Context, k LedgerKey, identity []byte, v LedgerVersion) error
	// Forget removes the entry after a successful delete.
	Forget(ctx context.Context, k LedgerKey, v LedgerVersion) error
}

// Ledger failures a caller must distinguish. A conflict is expected under concurrency and means
// re-read and retry; the others are programming or recovery errors.
var (
	// ErrLedgerConflict — the presented version is stale, or a create found the key present.
	ErrLedgerConflict = errors.New("ledger: version conflict")
	// ErrLedgerNotFound — no entry at the key.
	ErrLedgerNotFound = errors.New("ledger: entry not found")
	// ErrLedgerPhase — the transition is not legal from the entry's current phase.
	ErrLedgerPhase = errors.New("ledger: wrong phase")
)

// Merge decides, per field, what to send to the target. Three inputs from three sources: desired
// is the current resolved intent, prior is the last resolved intent (n-1), actual is a live read.
// Prior decides *ownership* — whether a silent field was dropped by us or belongs to someone else
// — and nothing else; actual decides drift.
//
// The returned mutation is a JSON merge document: a present value enforces that value, and an
// explicit null unsets the field.
type Merge interface {
	Apply(prior, desired, actual []byte) (mutation []byte, err error)
}

// KeySet names the keys a lease covers. Conflict is set intersection over a single object, so
// leases are never acquired one key at a time and acquisition order never arises — which is the
// only place a genuine deadlock could appear.
type KeySet interface {
	Contains(k LedgerKey) bool
	Intersects(other KeySet) bool
	// Keys enumerates the set, or reports false when the set is unbounded (v1 covers everything).
	Keys() ([]LedgerKey, bool)
	String() string
}

// Lease is the cross-run lock over a key set. It expires rather than requiring a manual unlock, so
// a dead holder frees the collection on its own.
//
// Expiry without fencing makes exclusivity advisory: a lease can lapse under a holder whose call
// is still in flight. Soundness would need the target to reject the stale holder, which cloud APIs
// generally cannot do, so the standing rule substitutes — read the target before acting on a
// broken lease.
type Lease interface {
	Holder() string
	Keys() KeySet
	Expiry() time.Time
	Renew(ctx context.Context, ttl time.Duration) error
	Release(ctx context.Context) error
}

// Leaser hands out leases within a scope.
type Leaser interface {
	Acquire(ctx context.Context, scope string, keys KeySet, holder string, ttl time.Duration) (Lease, error)
}

// ErrLeaseHeld — a live lease already covers part of the requested set.
var ErrLeaseHeld = errors.New("lease: held")

// JournalRecord is one appended step of a run, in the order the run reached it.
type JournalRecord interface {
	Seq() int
	Key() LedgerKey
	Exchange() string
	// Form is what the step did to the object. It decides the shape of the compensation, which the
	// exchange alone cannot: undoing a create is a delete, but undoing an update is a restore, and
	// deleting an object an earlier run created would be destruction rather than compensation.
	Form() FormClass
	// Prior is the object's resolved intent before this step, captured here because Resolve
	// promotes the new intent over it and the entry no longer carries it afterwards. Nil for a
	// create, which has nothing to restore.
	Prior() []byte
}

// Journal is the ordered, append-only forward log of a run. Unwind reverses it, which is why the
// order must be durable rather than reconstructed: a crashed run is torn down by a different
// process, with no plan graph in memory.
//
// It sits alongside the per-key Ledger rather than replacing it: keys hold state, the journal
// holds sequence.
type Journal interface {
	// Append records intent ahead of the wire effect and returns the assigned sequence.
	Append(ctx context.Context, k LedgerKey, exchange string, form FormClass, prior []byte) (int, error)
	// Records returns every entry in append order.
	Records(ctx context.Context) ([]JournalRecord, error)
}

// Journals opens the journal for a run.
type Journals interface {
	For(ctx context.Context, runID string) (Journal, error)
}

// Fidelity says how faithfully a compensation reverses its forward action. Lossy is the
// interesting case: it is what justifies preferring forward recovery, and what a policy gates on.
type Fidelity string

const (
	// FidelityExact — the inverse restores the prior state.
	FidelityExact Fidelity = "exact"
	// FidelityLossy — the inverse runs, but something does not come back: billing events, sent
	// notifications, consumed ids, deleted data.
	FidelityLossy Fidelity = "lossy"
	// FidelityNone — no inverse exists.
	FidelityNone Fidelity = "none"
)

// InverseExchange is the compensating exchange for a forward one, with how faithfully it
// reverses it.
type InverseExchange interface {
	Exchange() string
	Fidelity() Fidelity
}

// Semantics is the per-provider metadata that cannot be derived by differencing schemas. An
// exchange declares its inverse; the absence of one is the declaration of non-invertibility,
// which is constructive rather than a negative flag and also covers inverses that are not the
// obvious form — a disable undoing an enable.
type Semantics interface {
	Form(exchange string) (FormClass, bool)
	// Inverse reports the compensating exchange, or false where none is declared. This is
	// per-exchange, superseding FormClass.Inverse: create is generically invertible by delete, but
	// a particular create may not be — deletion protection, scheduled destruction, object-lock,
	// anything billable.
	Inverse(exchange string) (InverseExchange, bool)
	// Update reports the exchange that converges an object that already exists, or false where
	// none is declared. This is the verb-to-lifecycle role: without it there is no way to tell an
	// upsert, where re-issuing the create is correct, from a mint, where re-issuing produces a
	// duplicate. Absence means drift needs a replace, which is refused rather than guessed.
	Update(exchange string) (string, bool)
}

// EffectInput is everything one wire effect needs beyond the exchange itself.
type EffectInput struct {
	// Params address the object: the path or query parameters that name it. Some are supplied by
	// the caller and some bound from another key's recorded identity, which is what carries a
	// dependency that a β edge would carry inside a single plan.
	Params map[string]string
	// Mutation is what to send, as produced by a Merge. Nil for a read or a delete.
	Mutation []byte
	// Identity is the recorded identity of an object that already exists — set for an update or a
	// compensation, nil for a create.
	Identity []byte
}

// Effector performs one wire effect and returns the identity of the affected object: everything
// the inverse call needs to address it, not merely an id. It is the seam between the durable
// machinery and System-G's exchanges.
type Effector interface {
	Effect(ctx context.Context, exchange string, k LedgerKey, in EffectInput) (identity []byte, err error)
	// Read returns actual live state for a key, or readable=false where the target exposes no
	// usable read. Where it is false the log degrades from fact to belief — a property of the
	// target, not of the design.
	//
	// identity is the object's address as found on the target. When the caller supplied none, a
	// non-empty identity here means the object was rediscovered by its correlation key: the caller
	// may adopt it rather than create a duplicate, which is what keeps the ledger a cache instead
	// of the sole link to reality.
	Read(ctx context.Context, exchange string, k LedgerKey, in EffectInput) (actual, identity []byte, readable bool, err error)
}

// ErrCompensationBlocked marks a compensation that failed for a reason another compensation may
// clear — the provider refusing to delete a parent while children exist. Unwind retries these on a
// later pass rather than giving up, which is how provider referential integrity substitutes for
// dependency information we do not have.
var ErrCompensationBlocked = errors.New("compensation: blocked")

// Kind is what a type can DO with its values: parse one from its written form, compare two, and
// render one for the wire. It is a collection of functions, not a label — so dispatch is method
// lookup and adding a kind adds no switch anywhere.
//
// This is the seam that stops a numeric model being invented in the middle. A value crosses the
// system as the lexeme the provider wrote; whether that lexeme means an int64, a decimal or a
// timestamp is the document's statement, and only the kind acts on it.
type Kind interface {
	// Name identifies the kind for diagnostics.
	Name() string
	// Parse reads a value from its written form, rejecting a lexeme the kind cannot represent.
	// Bytes, not string: the wire and the ledger both deal in bytes, and a kind may hold something
	// that is not text at all.
	Parse(lexeme []byte) (Typed, error)
	// Compare orders two values of this kind: negative, zero or positive. It is an error where the
	// kind admits no order, which is not the same as the values being unequal.
	//
	// Comparable is false for a kind whose values are unbounded: answering "are these equal?" would
	// mean materialising a stream, so such values never participate in a merge and converge against
	// a digest the provider supplies instead.
	Comparable() bool
	Compare(a, b Typed) (int, error)
	// Encode renders a value in the form the wire expects.
	Encode(v Typed) ([]byte, error)
}

// Typed is a value that knows its own kind and keeps the form it was written in. That form is
// authoritative: converting to a machine number happens at the edge that needs one, never in
// transit, because that conversion is where 10000000000000001 becomes 10000000000000000.
//
// Reader is the primitive, not Lexeme. A value may be a stream from a firehose that never fits in
// memory, and a contract that can only hand back a slice would force materialising it.
type Typed interface {
	Kind() Kind
	// Reader pulls the value's bytes. Consumers set the pace; a fresh Reader starts from the
	// beginning where the kind is bounded, and an unbounded value may be read only once.
	Reader() io.Reader
	// Lexeme is the value exactly as written. Valid only where the kind is Comparable — an
	// unbounded value has no lexeme to hand back, and asking for one returns nil.
	Lexeme() []byte
}
