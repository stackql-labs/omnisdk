// Package trace carries a per-run log writer in the context so every exchange can log its
// traffic to one stream. No writer set → io.Discard (silent). This is how "log everything to
// begin with, silence later" is expressed: the driver sets the writer (or not).
package trace

import (
	"context"
	"io"
)

type ctxKey struct{}

// WithWriter returns ctx carrying w as the trace sink. A nil w leaves ctx unchanged (silent).
func WithWriter(ctx context.Context, w io.Writer) context.Context {
	if w == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, w)
}

// Writer returns the trace sink on ctx, or io.Discard if none.
func Writer(ctx context.Context) io.Writer {
	if w, ok := ctx.Value(ctxKey{}).(io.Writer); ok && w != nil {
		return w
	}
	return io.Discard
}
