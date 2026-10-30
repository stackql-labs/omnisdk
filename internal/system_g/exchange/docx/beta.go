package docx

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"

	"github.com/stackql-labs/omnisdk/internal/system_g/bind"
	"github.com/stackql-labs/omnisdk/internal/system_g/buffer"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/plan"
	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
	"github.com/stackql-labs/omnisdk/pkg/docparse/dsl"
)

// What is known about an operation whose x-stackQL-beta did not report done. Test with errors.Is.
var (
	// ErrAwaitFailed: the followed call reported done with an error. The operation failed.
	ErrAwaitFailed = errors.New("finished with an error")
	// ErrAwaitUnknown: the operation was accepted, and the followed call never reported done — it ran
	// out of attempts, or failed itself. What the operation made may or may not exist; read it.
	ErrAwaitUnknown = errors.New("did not report done, so whether it finished is unknown")
)

// awaitedSpec is an operation whose document declares it unfinished when it answers: each row it
// returns is followed by the declared call, polled until done, and the row emitted is that call's
// final answer. Anything downstream therefore runs only once what the operation made exists.
type awaitedSpec struct {
	plan.ExchangeSpec
	follow  plan.ExchangeSpec
	bind    map[string]string
	errPath string
}

func (a awaitedSpec) Make(bound map[string]any) facade.Operator {
	return awaitedOp{a: a, inner: a.ExchangeSpec.Make(bound), bound: bound}
}

// awaited compiles the operation's beta and wraps spec with it. The followed call is compiled with
// this call's credentials, endpoint and client, and with none of what shapes this call's own request
// or response: it is a different call.
func awaited(spec plan.ExchangeSpec, b aot.Beta, inputs map[string]any, reg dsl.Registry, o options) (plan.ExchangeSpec, error) {
	w := b.Await()
	fo := o
	fo.bound, fo.provided, fo.inbox, fo.bodyFields, fo.body = nil, nil, nil, nil, nil
	fo.objectKey, fo.mediaType, fo.programType, fo.programBody, fo.overrideResponse = "", "", "", "", false
	fo.ignoreResponse = false
	if len(b.Bind()) > 0 {
		fo.bound = map[string]bool{}
		for in := range b.Bind() {
			fo.bound[in] = true
		}
	}
	WithPoll(w.StatusPath(), w.Done(), w.Interval(), w.MaxAttempts())(&fo)
	follow, err := compile(b.Method(), inputs, reg, fo)
	if err != nil {
		return nil, fmt.Errorf("docx: x-stackQL-beta %s: %w", b.Method().Name(), err)
	}
	// The followed call carries the operation's own token, so its auth exchange is not needed.
	if c, ok := follow.(compiled); ok {
		follow = c.spec
	}
	return awaitedSpec{ExchangeSpec: spec, follow: follow, bind: b.Bind(), errPath: w.ErrorPath()}, nil
}

type awaitedOp struct {
	a     awaitedSpec
	inner facade.Operator
	bound map[string]any
}

func (o awaitedOp) Open(ctx context.Context) facade.Records {
	buf := buffer.NewBuffer(1, 16, 0)
	go func() {
		var err error
		defer func() { buf.Complete(err) }()
		rows := o.inner.Open(ctx)
		defer rows.Close()
		for rows.Next(ctx) {
			var done facade.Record
			if done, err = o.await(ctx, rows.Record()); err != nil {
				return
			}
			if err = buf.Append(ctx, done); err != nil {
				return
			}
		}
		err = rows.Err()
	}()
	return buf.Reader()
}

// await follows one row of the operation to its done answer. The followed call is bound with what
// the operation was bound with — its scope and its token — and with each declared field of the row.
func (o awaitedOp) await(ctx context.Context, row facade.Record) (facade.Record, error) {
	doc, _ := bind.DocMap(row)
	in := maps.Clone(o.bound)
	for name, field := range o.a.bind {
		v, ok := at(doc, field)
		if !ok {
			return nil, fmt.Errorf("docx: %s answered without %s, which its x-stackQL-beta reads: %w", o.a.Name(), field, ErrAwaitUnknown)
		}
		in[name] = v
	}
	rows := o.a.follow.Make(in).Open(ctx)
	defer rows.Close()
	var last facade.Record
	for rows.Next(ctx) {
		last = rows.Record()
	}
	if err := rows.Err(); err != nil {
		// %v, not %w: the followed call's own failure — a 4xx included — says nothing about whether
		// the operation took effect, and must not read as the operation being rejected.
		return nil, fmt.Errorf("docx: %s %w: %v", o.a.Name(), ErrAwaitUnknown, err)
	}
	if last == nil {
		return nil, fmt.Errorf("docx: %s %w: its x-stackQL-beta returned nothing", o.a.Name(), ErrAwaitUnknown)
	}
	if o.a.errPath != "" {
		final, _ := bind.DocMap(last)
		if e, ok := at(final, o.a.errPath); ok && e != nil {
			return nil, fmt.Errorf("docx: %s %w: %v", o.a.Name(), ErrAwaitFailed, e)
		}
	}
	final, ok := bind.DocMap(last)
	if !ok {
		return nil, fmt.Errorf("docx: %s %w: final response is not an object", o.a.Name(), ErrAwaitUnknown)
	}
	merged := maps.Clone(doc)
	if merged == nil {
		merged = map[string]any{}
	}
	maps.Copy(merged, final)
	return bind.NewDocRecord(merged), nil
}

// at walks a dotted path into a decoded document.
func at(doc map[string]any, path string) (any, bool) {
	var cur any = doc
	for _, step := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[step]; !ok {
			return nil, false
		}
	}
	return cur, true
}
