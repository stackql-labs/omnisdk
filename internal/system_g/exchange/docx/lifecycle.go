package docx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/stackql-labs/omnisdk/internal/system_g/bind"
	"github.com/stackql-labs/omnisdk/internal/system_g/buffer"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/plan"
	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
	"github.com/stackql-labs/omnisdk/pkg/docparse/dsl"
)

type lifecycleSpec struct {
	plan.ExchangeSpec
	follow        plan.ExchangeSpec
	declaration   *aot.Lifecycle
	required      []string
	selfParameter string
	requestName   any
}

func lifecycleAwaited(spec plan.ExchangeSpec, declaration *aot.Lifecycle, inputs map[string]any, reg dsl.Registry, o options) (plan.ExchangeSpec, error) {
	fo := o
	fo.bound, fo.provided, fo.inbox, fo.bodyFields, fo.body = nil, nil, nil, nil, nil
	fo.objectKey, fo.mediaType, fo.programType, fo.programBody, fo.overrideResponse = "", "", "", "", false
	fo.wholeResponse, fo.ignoreResponse, fo.snapshotCache = false, false, false
	fo.poll = nil
	fo.observationOnly = true
	var required []string
	selfParameter := ""
	lastPosition := -1
	for _, parameter := range declaration.Method.Request().Parameters() {
		if parameter.In() == aot.InPath {
			position := strings.LastIndex(declaration.Method.Request().URL(), "{"+parameter.Name()+"}")
			if position > lastPosition {
				selfParameter = parameter.Name()
				lastPosition = position
			}
		}
		if parameter.Required() {
			required = append(required, parameter.Name())
		}
	}
	WithBound(required...)(&fo)
	for name := range declaration.Bind {
		WithBound(name)(&fo)
	}
	follow, err := compile(declaration.Method, inputs, reg, fo)
	if err != nil {
		return nil, err
	}
	if c, ok := follow.(compiled); ok {
		follow = c.spec
	}
	var body map[string]any
	_ = json.Unmarshal(o.body, &body)
	return lifecycleSpec{ExchangeSpec: spec, follow: follow, declaration: declaration, required: required, selfParameter: selfParameter, requestName: body["name"]}, nil
}

func (s lifecycleSpec) Make(bound map[string]any) facade.Operator {
	return lifecycleOp{s: s, inner: s.ExchangeSpec.Make(bound), bound: bound}
}

type lifecycleOp struct {
	s     lifecycleSpec
	inner facade.Operator
	bound map[string]any
}

func (o lifecycleOp) Open(ctx context.Context) facade.Records {
	buf := buffer.NewBuffer(1, 16, 0)
	go func() {
		var err error
		defer func() { buf.Complete(err) }()
		rows := o.inner.Open(ctx)
		defer rows.Close()
		for rows.Next(ctx) {
			row := rows.Record()
			if row, err = o.await(ctx, row); err != nil {
				return
			}
			if err = buf.Append(ctx, row); err != nil {
				return
			}
		}
		err = rows.Err()
	}()
	return buf.Reader()
}

func (o lifecycleOp) await(ctx context.Context, row facade.Record) (facade.Record, error) {
	doc, ok := bind.DocMap(row)
	if !ok {
		return nil, fmt.Errorf("lifecycle: create returned no object: %w", ErrAwaitUnknown)
	}
	in := maps.Clone(o.bound)
	if in == nil {
		in = map[string]any{}
	}
	for _, name := range o.s.required {
		if value, exists := doc[name]; exists && value != nil {
			in[name] = value
		}
	}
	// The final path parameter identifies a detail object. A caller-chosen
	// request name is evidence; an operation's response name is not substituted.
	if name := o.s.selfParameter; name != "" && o.bound[name] == nil {
		identity := o.s.requestName
		if identity == nil {
			identity = o.bound["name"]
		}
		if identity != nil {
			in[name] = identity
		}
	}
	for name, field := range o.s.declaration.Bind {
		value, exists := at(doc, field)
		if !exists || value == nil {
			return nil, fmt.Errorf("lifecycle: create returned no %s: %w", field, ErrAwaitUnknown)
		}
		in[name] = value
	}
	for _, name := range o.s.required {
		if in[name] == nil || fmt.Sprint(in[name]) == "" {
			return nil, fmt.Errorf("lifecycle: self read has no handle for %s: %w", name, ErrAwaitUnknown)
		}
	}
	names := append([]string(nil), o.s.required...)
	for _, name := range o.s.follow.In() {
		if in[name] != nil {
			names = append(names, name)
		}
	}
	for name := range o.s.declaration.Bind {
		names = append(names, name)
	}
	target := o.s.declaration.Method.Request().Method() + " " + o.s.declaration.Method.Request().URL()
	if err := checkpointAwait(ctx, target, in, names); err != nil {
		return nil, err
	}
	for attempt := 0; attempt < o.s.declaration.MaxAttempts; attempt++ {
		rows := o.s.follow.Make(in).Open(ctx)
		found := false
		for rows.Next(ctx) {
			observation, ok := bind.DocMap(rows.Record())
			if ok && len(observation) > 0 {
				found = true
			}
		}
		err := rows.Err()
		rows.Close()
		if err != nil {
			var status facade.HTTPStatusError
			if !errors.As(err, &status) || status.StatusCode() != 404 {
				return nil, fmt.Errorf("lifecycle: self read: %w: %v", ErrAwaitUnknown, err)
			}
		} else if found {
			return row, nil
		}
		if attempt+1 < o.s.declaration.MaxAttempts {
			timer := time.NewTimer(o.s.declaration.Interval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, fmt.Errorf("lifecycle: %w: %v", ErrAwaitUnknown, ctx.Err())
			case <-timer.C:
			}
		}
	}
	return nil, fmt.Errorf("lifecycle: self read returned no data after %d attempts: %w", o.s.declaration.MaxAttempts, ErrAwaitUnknown)
}

// AwaitCheckpoint records only the addressing inputs needed to resume an observation.
// It is invoked after create answers and before the first observation request.
type AwaitCheckpoint func(context.Context, string, map[string]any) error
type awaitCheckpointKey struct{}

func WithAwaitCheckpoint(ctx context.Context, checkpoint AwaitCheckpoint) context.Context {
	return context.WithValue(ctx, awaitCheckpointKey{}, checkpoint)
}

func checkpointAwait(ctx context.Context, method string, inputs map[string]any, names []string) error {
	checkpoint, _ := ctx.Value(awaitCheckpointKey{}).(AwaitCheckpoint)
	if checkpoint == nil {
		return nil
	}
	handles := map[string]any{}
	for _, name := range names {
		if !IsCredential(name) {
			handles[name] = inputs[name]
		}
	}
	if err := checkpoint(ctx, method, handles); err != nil {
		return fmt.Errorf("await handle checkpoint: %w: %v", ErrAwaitUnknown, err)
	}
	return nil
}

func observationScope(o options, bound map[string]any) map[string]any {
	if o.observationOnly {
		return nil
	}
	return bound
}
