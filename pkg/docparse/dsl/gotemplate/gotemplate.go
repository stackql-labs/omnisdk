// Package gotemplate implements the Go-template languages a stackql provider document embeds. Two
// versions ship in the documents and they differ ONLY in what "." is bound to:
//
//	golang_template_text_v0.3.0  "." is the input as a STRING — request shaping, where the program
//	                             builds a form query from a JSON request context.
//	golang_template_mxj_v0.2.0   "." is the input decoded from XML to a MAP (mxj) — response shaping,
//	                             where the program rewrites a provider's XML into the document's own
//	                             JSON.
//
// Everything else — the helper library, the escaping rules — is shared, so the difference stays the
// one line it actually is.
package gotemplate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"text/template"

	"github.com/clbanning/mxj/v2"

	"github.com/stackql-labs/omnisdk/pkg/docparse/dsl"
	"github.com/stackql-labs/omnisdk/pkg/docparse/dsl/gotemplate/internal/funcs"
)

// Declared type identifiers, exactly as documents spell them.
const (
	TypeText  = "golang_template_text_v0.3.0"
	TypeMXJ   = "golang_template_mxj_v0.2.0"
	TypeMXJ3  = "golang_template_mxj_v0.3.0"
	TypeJSON1 = "golang_template_json_v0.1.0"
	TypeJSON3 = "golang_template_json_v0.3.0"
)

// Text is the request-shaping language: "." is the raw input as a string.
func Text() dsl.Evaluator { return evaluator{typ: TypeText, bind: bindString} }

// MXJ is the response-shaping language: "." is the input decoded from XML into a map. The versions
// differ in the documents' own numbering, not in what "." is bound to, so one implementation serves
// both — registering them separately keeps a document's declared version honest rather than silently
// accepting anything mxj-shaped.
func MXJ() dsl.Evaluator  { return evaluator{typ: TypeMXJ, bind: bindXML} }
func MXJ3() dsl.Evaluator { return evaluator{typ: TypeMXJ3, bind: bindXML} }

// JSON binds "." to the input decoded from JSON — the same language over an already-structured body,
// which is what the majority of documents shape their responses with.
func JSON1() dsl.Evaluator { return evaluator{typ: TypeJSON1, bind: bindJSON} }
func JSON3() dsl.Evaluator { return evaluator{typ: TypeJSON3, bind: bindJSON} }

// Evaluators is every language this package implements, for building a registry.
func Evaluators() []dsl.Evaluator {
	return []dsl.Evaluator{Text(), MXJ(), MXJ3(), JSON1(), JSON3()}
}

type evaluator struct {
	typ  string
	bind func([]byte) (any, error)
}

func (e evaluator) Type() string { return e.typ }

func (e evaluator) Eval(program string, in []byte) ([]byte, error) {
	dot, err := e.bind(in)
	if err != nil {
		return nil, err
	}
	// Option("missingkey=zero"): documents index into optional response fields and rely on the miss
	// being falsy for `with`/`if`, rather than erroring. Erroring would make an absent optional field
	// a failure instead of an answer.
	t, err := template.New(e.typ).Funcs(funcs.Map()).Option("missingkey=zero").Parse(program)
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	var out strings.Builder
	if err := t.Execute(&out, dot); err != nil {
		return nil, fmt.Errorf("execute: %w", err)
	}
	return []byte(out.String()), nil
}

// bindString binds "." to the input as a string.
func bindString(in []byte) (any, error) { return string(in), nil }

// bindJSON binds "." to the input decoded from JSON. An empty body is an empty document, not a
// parse failure: a program may legitimately run over a response that carried nothing.
func bindJSON(in []byte) (any, error) {
	if len(bytes.TrimSpace(in)) == 0 {
		return map[string]any{}, nil
	}
	var v any
	if err := json.Unmarshal(in, &v); err != nil {
		return nil, fmt.Errorf("decode json: %w", err)
	}
	return v, nil
}

// bindXML binds "." to the input decoded from XML into a map. mxj is what the language is named for
// and what the documents are written against — its convention that a repeated element becomes a
// slice while a single one stays a scalar is exactly why the programs branch on kindOf.
func bindXML(in []byte) (any, error) {
	m, err := mxj.NewMapXml(in)
	if err != nil {
		return nil, fmt.Errorf("decode xml: %w", err)
	}
	return map[string]any(m), nil
}
