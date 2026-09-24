package omnisdk

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"

	"github.com/stackql-labs/omnisdk/internal/system_g/bind"
	"github.com/stackql-labs/omnisdk/internal/system_g/buffer"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/fn"
	"github.com/stackql-labs/omnisdk/internal/system_g/plan"
	"github.com/stackql-labs/omnisdk/pkg/query"
)

// What a request cannot do, done on the rows: filters, IN lists and joins neither side needs.

// predicateColumns are the columns a predicate reads.
func predicateColumns(p query.Predicate) []query.Column {
	var out []query.Column
	var expr func(query.Expr)
	expr = func(e query.Expr) {
		switch e := e.(type) {
		case query.Column:
			out = append(out, e)
		case query.Collection:
			for _, x := range e.Items() {
				expr(x)
			}
		case query.Call:
			for _, x := range e.Args() {
				expr(x)
			}
		}
	}
	var pred func(query.Predicate)
	pred = func(p query.Predicate) {
		switch p := p.(type) {
		case query.Compare:
			expr(p.Left())
			expr(p.Right())
		case query.In:
			expr(p.Expr())
			expr(p.Set())
		case query.Test:
			expr(p.Cond())
		case query.Or:
			for _, q := range p.Any() {
				pred(q)
			}
		case query.Not:
			pred(p.Negated())
		}
	}
	pred(p)
	return out
}

// truth is SQL's three-valued logic: a comparison with a missing value is unknown, and only a true
// condition keeps a row.
type truth int

const (
	unknown truth = iota
	isFalse
	isTrue
)

func truthOf(b bool) truth {
	if b {
		return isTrue
	}
	return isFalse
}

// filterTransform keeps the rows every filter holds for. It reads each column from the key private
// to its node, so a filter over two references to one address compares the right two values.
type filterTransform struct {
	filters []query.Predicate
	fns     facade.FnRegistry
}

func (f filterTransform) Apply(in facade.Page) (facade.Record, error) {
	row, ok := bind.DocMap(in)
	if !ok {
		if rec, is := in.(facade.Record); is {
			return rec, nil
		}
		return nil, fmt.Errorf("omnisdk: filter received a %T, not a record", in)
	}
	for _, p := range f.filters {
		t, err := f.eval(p, row)
		if err != nil {
			return nil, err
		}
		if t != isTrue {
			return nil, nil
		}
	}
	return bind.NewDocRecord(row), nil
}

func (f filterTransform) eval(p query.Predicate, row map[string]any) (truth, error) {
	switch p := p.(type) {
	case query.Compare:
		l, err := f.value(p.Left(), row)
		if err != nil {
			return unknown, err
		}
		r, err := f.value(p.Right(), row)
		if err != nil {
			return unknown, err
		}
		c, ok := compareValues(l, r)
		if !ok {
			return unknown, nil
		}
		switch p.Op() {
		case query.Eq:
			return truthOf(c == 0), nil
		case query.Ne:
			return truthOf(c != 0), nil
		case query.Lt:
			return truthOf(c < 0), nil
		case query.Le:
			return truthOf(c <= 0), nil
		case query.Gt:
			return truthOf(c > 0), nil
		case query.Ge:
			return truthOf(c >= 0), nil
		}
		return unknown, fmt.Errorf("omnisdk: unknown comparison %q", p.Op())
	case query.In:
		v, err := f.value(p.Expr(), row)
		if err != nil {
			return unknown, err
		}
		set, ok := p.Set().(query.Collection)
		if !ok {
			return unknown, fmt.Errorf("omnisdk: IN needs a list")
		}
		result := isFalse
		for _, x := range set.Items() {
			w, err := f.value(x, row)
			if err != nil {
				return unknown, err
			}
			c, ok := compareValues(v, w)
			switch {
			case !ok:
				result = unknown
			case c == 0:
				return isTrue, nil
			}
		}
		return result, nil
	case query.Test:
		v, err := f.value(p.Cond(), row)
		if err != nil {
			return unknown, err
		}
		b, ok := v.(bool)
		if !ok {
			return unknown, nil
		}
		return truthOf(b), nil
	case query.Or:
		result := isFalse
		for _, q := range p.Any() {
			t, err := f.eval(q, row)
			if err != nil {
				return unknown, err
			}
			switch t {
			case isTrue:
				return isTrue, nil
			case unknown:
				result = unknown
			}
		}
		return result, nil
	case query.Not:
		t, err := f.eval(p.Negated(), row)
		switch t {
		case isTrue:
			return isFalse, err
		case isFalse:
			return isTrue, err
		}
		return unknown, err
	}
	return unknown, fmt.Errorf("omnisdk: unsupported condition %T", p)
}

func (f filterTransform) value(e query.Expr, row map[string]any) (any, error) {
	x, err := engineExpr(e, func(c query.Column) fn.Expr { return fn.Field(hidden(c.Qualifier(), c.Name())) })
	if err != nil {
		return nil, err
	}
	return x.Eval(row, f.fns)
}

// engineExpr converts a query expression to the engine's, with column resolved by the caller.
func engineExpr(e query.Expr, column func(query.Column) fn.Expr) (fn.Expr, error) {
	switch e := e.(type) {
	case query.Literal:
		return fn.Literal(e.Value()), nil
	case query.Column:
		return column(e), nil
	case query.Call:
		args := make([]fn.Expr, 0, len(e.Args()))
		for _, a := range e.Args() {
			x, err := engineExpr(a, column)
			if err != nil {
				return nil, err
			}
			args = append(args, x)
		}
		return fn.Call(e.Func(), args...), nil
	}
	return nil, fmt.Errorf("%T is not a value", e)
}

// compareValues orders two values: numerically where both read as numbers, otherwise as text.
// A missing value compares with nothing.
func compareValues(a, b any) (int, bool) {
	if a == nil || b == nil {
		return 0, false
	}
	as, bs := fmt.Sprint(a), fmt.Sprint(b)
	if af, err := strconv.ParseFloat(as, 64); err == nil {
		if bf, err := strconv.ParseFloat(bs, 64); err == nil {
			switch {
			case af < bf:
				return -1, true
			case af > bf:
				return 1, true
			}
			return 0, true
		}
	}
	switch {
	case as < bs:
		return -1, true
	case as > bs:
		return 1, true
	}
	return 0, true
}

// valuesSpec is an exchange that makes no request: it emits one row per combination of
// multi-valued inputs, so whatever binds them runs once for each. With an alias the values sit
// under that node's private keys; without one they are query-wide and sit under their own names.
func valuesSpec(name, alias string, fanout map[string][]string) plan.ExchangeSpec {
	key := func(k string) string { return k }
	if alias != "" {
		key = func(k string) string { return hidden(alias, k) }
	}
	var rows []map[string]any
	rows = append(rows, map[string]any{})
	var out []string
	for k, vs := range fanout {
		out = append(out, key(k))
		next := make([]map[string]any, 0, len(rows)*len(vs))
		for _, r := range rows {
			for _, v := range vs {
				c := make(map[string]any, len(r)+1)
				for rk, rv := range r {
					c[rk] = rv
				}
				c[key(k)] = v
				next = append(next, c)
			}
		}
		rows = next
	}
	return plan.NewExchangeSpec(name, nil, out, func(map[string]any) facade.Operator {
		return staticOp{rows: rows}
	}, nil)
}

type staticOp struct{ rows []map[string]any }

func (s staticOp) Open(ctx context.Context) facade.Records {
	buf := buffer.NewBuffer(1, len(s.rows)+1, 0)
	go func() {
		var err error
		defer func() { buf.Complete(err) }()
		for _, r := range s.rows {
			if err = buf.Append(ctx, bind.NewDocRecord(r)); err != nil {
				return
			}
		}
	}()
	return buf.Reader()
}

// replayed runs a node once per distinct input and replays its rows to every later caller in the
// same run. A node nothing is wired into binds the same inputs for every upstream row, so without
// this a join neither side needs would re-list it once per row of the other.
type replayed struct{ plan.ExchangeSpec }

func (r replayed) Make(bound map[string]any) facade.Operator {
	return replayOp{spec: r.ExchangeSpec, bound: bound}
}

type replayOp struct {
	spec  plan.ExchangeSpec
	bound map[string]any
}

func (o replayOp) Open(ctx context.Context) facade.Records {
	store, _ := ctx.Value(replayKey{}).(*replayStore)
	key, err := json.Marshal(o.bound)
	if store == nil || err != nil {
		return o.spec.Make(o.bound).Open(ctx)
	}
	return store.entry(o.spec.Name()+"\x00"+string(key), func() facade.Records {
		return o.spec.Make(o.bound).Open(ctx)
	}).reader()
}

type replayKey struct{}

// withReplay gives a run its own replay store: rows are shared within one run, never across runs.
func withReplay(ctx context.Context) context.Context {
	return context.WithValue(ctx, replayKey{}, &replayStore{entries: map[string]*replayEntry{}})
}

type replayStore struct {
	mu      sync.Mutex
	entries map[string]*replayEntry
}

// entry returns the recording for key, starting it on first use.
func (s *replayStore) entry(key string, open func() facade.Records) *replayEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.entries[key]; ok {
		return e
	}
	e := &replayEntry{}
	e.cond = sync.NewCond(&e.mu)
	s.entries[key] = e
	go e.record(open)
	return e
}

// replayEntry records one run of a node as its rows arrive, so a reader streams them as they come
// rather than waiting for the whole listing.
type replayEntry struct {
	mu   sync.Mutex
	cond *sync.Cond
	recs []facade.Record
	done bool
	err  error
}

func (e *replayEntry) record(open func() facade.Records) {
	in := open()
	defer in.Close()
	for in.Next(context.Background()) {
		e.mu.Lock()
		e.recs = append(e.recs, in.Record())
		e.mu.Unlock()
		e.cond.Broadcast()
	}
	e.mu.Lock()
	e.done, e.err = true, in.Err()
	e.mu.Unlock()
	e.cond.Broadcast()
}

func (e *replayEntry) reader() facade.Records { return &replayReader{e: e} }

type replayReader struct {
	e   *replayEntry
	i   int
	cur facade.Record
}

func (r *replayReader) Next(ctx context.Context) bool {
	e := r.e
	// A waiting reader must still notice its own cancellation.
	stop := context.AfterFunc(ctx, func() {
		e.mu.Lock()
		e.cond.Broadcast()
		e.mu.Unlock()
	})
	defer stop()
	e.mu.Lock()
	defer e.mu.Unlock()
	for r.i >= len(e.recs) && !e.done && ctx.Err() == nil {
		e.cond.Wait()
	}
	if r.i < len(e.recs) {
		r.cur = e.recs[r.i]
		r.i++
		return true
	}
	return false
}

func (r *replayReader) Record() facade.Record { return r.cur }

func (r *replayReader) Err() error {
	r.e.mu.Lock()
	defer r.e.mu.Unlock()
	return r.e.err
}

func (r *replayReader) Close() error { return nil }
