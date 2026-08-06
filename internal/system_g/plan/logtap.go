package plan

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"

	"github.com/stackql-labs/omnisdk/internal/system_g/buffer"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/trace"
)

// logTap is a passthrough logging OPERATOR (not a transform): as records stream through it, it
// writes each to the run's log sink — the writer on the context, io.Discard = silent — then
// forwards it unchanged. One tap per exchange, so the output sees (and logs) every exchange's
// output, per the bowtie (every exchange has a β edge to Output). Secrets are redacted.
//
// Logging is a side effect that needs the per-run ambient writer, so it belongs to an operator
// (which receives ctx in Open), never to a Transform — transforms are pure page→page maps with
// no ctx and no source. See facade.Transform.
type logTap struct {
	id       int64
	upstream facade.Operator
	label    string
	readers  int
}

// NewLogTap wraps upstream in a tap that logs each record under label and forwards it.
func NewLogTap(id int64, upstream facade.Operator, label string, readers int) facade.Operator {
	return &logTap{id: id, upstream: upstream, label: label, readers: readers}
}

func (t *logTap) Open(ctx context.Context) facade.Records {
	readers := t.readers
	if readers < 1 {
		readers = 1
	}
	buf := buffer.NewBuffer(readers, 1024, 0)
	w := trace.Writer(ctx)
	in := t.upstream.Open(ctx)
	go func() {
		var cerr error
		defer func() { buf.Complete(cerr) }()
		defer in.Close()
		for in.Next(ctx) {
			rec := in.Record()
			if w != io.Discard {
				if v := rec.Get(facade.AnonymousPayload); v != nil {
					b, _ := io.ReadAll(v.Reader())
					fmt.Fprintf(w, "%s: %s\n", t.label, Redact(b))
				}
			}
			if err := buf.Append(ctx, rec); err != nil {
				if !errors.Is(err, buffer.ErrAllReadersClosed) {
					cerr = err
				}
				return
			}
		}
		cerr = in.Err()
	}()
	return buf.Reader()
}

// Redact is the log redaction seam. PROVISIONAL: a stopgap scrubber, swappable via this var,
// until redaction becomes a first-class facade policy object (per-field/per-role policies).
// Do not entrench callers on the current regex behaviour.
var Redact = scrubSecrets

// scrubSecrets is the default stopgap: blanks tokens and jwt-bearer assertions so they never
// reach a log file. Not a policy — a placeholder.
// reSecretField matches any JSON string field whose key looks secret (token, secret, password,
// key, assertion, credential) — so provider secrets flowing as κ inputs never hit a log.
var (
	reSecretField = regexp.MustCompile(`(?i)("[a-z0-9_]*(?:secret|token|password|assertion|credential|private_key)[a-z0-9_]*"\s*:\s*)"[^"]*"`)
	reAssertion   = regexp.MustCompile(`assertion=[^&\s"]+`)
	reBearer      = regexp.MustCompile(`Bearer [A-Za-z0-9._\-]+`)
)

func scrubSecrets(b []byte) []byte {
	b = reSecretField.ReplaceAll(b, []byte(`$1"«redacted»"`))
	b = reAssertion.ReplaceAll(b, []byte("assertion=«redacted»"))
	b = reBearer.ReplaceAll(b, []byte("Bearer «redacted»"))
	return b
}
