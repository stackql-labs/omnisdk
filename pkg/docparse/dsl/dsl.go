// Package dsl is the abstraction over the little languages a provider document embeds. A document
// does not describe its request and response shaping in data — it ships PROGRAMS, each tagged with
// the evaluator that runs it (golang_template_mxj_v0.2.0, golang_template_text_v0.3.0, …). This
// package is the seam that keeps that fact contained: a caller resolves a declared type to an
// Evaluator and runs it, and never learns which language it was.
//
// The registry is EXPLICIT — evaluators are passed in, not self-registered from init(). A document
// naming a language nothing implements must fail loudly and say so, and that is only possible when
// the set of supported languages is a decision the caller makes rather than a side effect of which
// packages happen to be linked in.
package dsl

import (
	"fmt"
	"sort"
)

// Evaluator runs one embedded language.
type Evaluator interface {
	// Type is the identifier a document uses to name this language, version included.
	Type() string
	// Eval runs program over in and returns the transformed document. Both sides are bytes: what a
	// program consumes and produces is the language's business, not the caller's.
	Eval(program string, in []byte) ([]byte, error)
}

// Registry resolves a declared type to its evaluator.
type Registry interface {
	// Get returns the evaluator for a declared type.
	Get(typ string) (Evaluator, bool)
	// Types are the languages this registry supports, sorted — so an error can say what IS supported.
	Types() []string
	// Eval resolves and runs in one step, failing with a message naming the unsupported type.
	Eval(typ, program string, in []byte) ([]byte, error)
}

// NewRegistry builds a registry over the given evaluators. A duplicate type is a programming error:
// two implementations of one language means the document's meaning depends on link order.
func NewRegistry(evaluators ...Evaluator) (Registry, error) {
	r := registry{}
	for _, e := range evaluators {
		if _, dup := r[e.Type()]; dup {
			return nil, fmt.Errorf("dsl: duplicate evaluator for %q", e.Type())
		}
		r[e.Type()] = e
	}
	return r, nil
}

type registry map[string]Evaluator

func (r registry) Get(typ string) (Evaluator, bool) {
	e, ok := r[typ]
	return e, ok
}

func (r registry) Types() []string {
	out := make([]string, 0, len(r))
	for t := range r {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

func (r registry) Eval(typ, program string, in []byte) ([]byte, error) {
	e, ok := r[typ]
	if !ok {
		return nil, fmt.Errorf("dsl: no evaluator for %q (have: %v)", typ, r.Types())
	}
	out, err := e.Eval(program, in)
	if err != nil {
		return nil, fmt.Errorf("dsl: %s: %w", typ, err)
	}
	return out, nil
}
