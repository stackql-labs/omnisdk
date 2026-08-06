# buffer

Single-writer, multi-reader, append-only log on a β edge. One producer appends
immutable rows; each consumer pulls via its own `Rows` cursor. Backed by non-moving
chunks so reads never race a realloc.

## Interfaces (facade)

```go
Buffer:  Append(ctx, Row) error   // publish one row; blocks under back-pressure
         Complete(err error)       // EOF; no Append after
         Reader() Rows             // fresh cursor per consumer

Rows:    Next(ctx) bool            // advance; blocks at tail; false on EOF/err/cancel
         Row() Row                 // current row (valid after Next==true)
         Err() error               // terminal error (after Next==false)
         Close() error             // release this cursor's hold

Row:     At(i) Value / Len()       // read-only, values fixed at construction
```

## Mechanism

- **Publish:** writer fills slot, then release-stores `length`. Readers acquire-load
  `length` and index below it → no torn reads, no RW lock.
- **Tail wait:** reader caught up to `length` parks on `wake`; writer broadcasts on each
  append and on `Complete`.
- **Storage:** `chunks [][]Row`, `chunkSize` per chunk. Existing chunks never move;
  append extends tail chunk or adds one.
- **Release:** `cursors[]` = per-reader position; `min(cursors)` = low-water mark.
  Rows below it are dead → nil the slot, advance `released`. `readers` = static count
  from DAG out-degree.

## Producer contract

1. `Append(ctx, row)` per row. Build the row fully before append (immutable after).
2. Append blocks when the buffer is full (back-pressure); honor `ctx` cancel.
3. Call `Complete(nil)` at EOF, or `Complete(err)` on failure. Exactly once.

## Consumer contract

1. Get a cursor: `r := b.Reader()`.
2. Loop `for r.Next(ctx) { use(r.Row()) }`.
3. `Next==false` → check `r.Err()`.
4. Always `r.Close()` (releases refs so rows can be reclaimed).
5. Never mutate a `Row`.

## Unbounded sources

`Complete` never arrives, so **do not gate release on completion.** Two independent
knobs keep memory bounded:

- **Release** always runs at the low-water mark (slowest cursor), independent of EOF.
  A row is freed once every reader has passed it — works the same for infinite streams.
- **Back-pressure** caps live (unreleased) rows; a full buffer blocks `Append` until the
  slowest consumer advances. The fast branch is throttled to the slow branch's rate.

Termination is a **policy** the producer enforces, not the buffer: with a well-founded
policy (TTL, max rows, idle timeout, cycle bound) configured, the producer stops pulling
its source, emits `Complete`, and the stream ends. The buffer only provides the bounded
window and the release/EOF signals; deciding *when to stop* is the policy's job.

**Deadlock note:** a blocking consumer (sort/aggregate) above an unbounded fan-out needs
EOF that back-pressure prevents → don't place blocking operators over a capped fan-out.
