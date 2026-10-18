// Package apply decides whether a live object already is what a mutation asks for — the test a
// converging run applies before it sends anything.
//
// Intent is opaque everywhere else; here it is read as a JSON document, with numbers kept as the text
// they were written as.
package apply

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Satisfied reports whether actual already satisfies mutation: every enforced field matches, and every
// unset field is absent. It is the test for "nothing to do".
func Satisfied(mutation, actual []byte) (bool, error) { return converged(mutation, actual) }

// converged reports whether actual already satisfies mutation: every enforced field matches, and
// every unset field is absent. It is the test for "nothing to do", and it is why a re-run costs no
// wire calls.
func converged(mutation, actual []byte) (bool, error) {
	want, err := decodeDoc(mutation)
	if err != nil {
		return false, fmt.Errorf("apply: mutation is not a JSON object: %w", err)
	}
	have, err := decodeDoc(actual)
	if err != nil {
		// An unreadable actual is not a match; converging on a target we cannot read is the case
		// the design already calls belief rather than fact.
		return false, nil
	}
	return satisfied(want, have), nil
}

// decodeDoc reads a document keeping numbers as their source text, so comparison is between what
// was written and what came back rather than between two float64 approximations of them.
func decodeDoc(b []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var o map[string]any
	if err := dec.Decode(&o); err != nil {
		return nil, err
	}
	return o, nil
}

func satisfied(want, have map[string]any) bool {
	for k, v := range want {
		got, present := have[k]
		if v == nil {
			if present {
				return false
			}
			continue
		}
		if !present {
			return false
		}
		wsub, wok := v.(map[string]any)
		gsub, gok := got.(map[string]any)
		if wok && gok {
			if !satisfied(wsub, gsub) {
				return false
			}
			continue
		}
		if fmt.Sprint(v) != fmt.Sprint(got) {
			return false
		}
	}
	return true
}
