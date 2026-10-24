package omnisdk

import (
	"fmt"
	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
	"strings"
)

// InverseToken describes an explicitly declared response-to-inverse binding.
type InverseToken struct {
	Key       string `yaml:"key"`
	Location  string `yaml:"location"`
	Algorithm string `yaml:"algorithm"`
}

// DeclaredInverse is document metadata, never an inferred destructive operation.
type DeclaredInverse interface {
	Method() string
	Tokens() map[string]InverseToken
}

// DocInverse returns a mutation's declared inverse, if present.
func DocInverse(dir, address, verb string) (DeclaredInverse, error) {
	c, err := openDocs(dir, address)
	if err != nil {
		return nil, err
	}
	ops, err := c.Operations(address, verb)
	if err != nil {
		return nil, err
	}
	if len(ops) != 1 {
		return nil, fmt.Errorf("inverse requires an unambiguous mutation method")
	}
	for _, op := range ops {
		if declared, ok := op.(aot.InverseDeclaration); ok {
			ref, tokens := declared.InverseDeclaration()
			if ref == "" {
				continue
			}
			method := ref[strings.LastIndex(ref, "/")+1:]
			resource := address[strings.LastIndex(address, ".")+1:]
			if !strings.HasSuffix(ref, "/"+resource+"/methods/"+method) {
				return nil, fmt.Errorf("inverse targets a different resource")
			}
			deletes, err := c.Operations(address, "delete")
			if err != nil {
				return nil, err
			}
			found := false
			for _, candidate := range deletes {
				if candidate.Name() == method {
					found = true
					break
				}
			}
			if !found {
				return nil, fmt.Errorf("inverse method %s is not a declared delete", method)
			}
			out := map[string]InverseToken{}
			for name, t := range tokens {
				out[name] = InverseToken{Key: t[0], Location: t[1], Algorithm: t[2]}
			}
			return inverseDeclaration{method: method, tokens: out}, nil
		}
	}
	return nil, nil
}

type inverseDeclaration struct {
	method string
	tokens map[string]InverseToken
}

func (d inverseDeclaration) Method() string { return d.method }
func (d inverseDeclaration) Tokens() map[string]InverseToken {
	out := map[string]InverseToken{}
	for n, t := range d.tokens {
		out[n] = t
	}
	return out
}

// NewInverseMutationNode pins the explicitly declared inverse method.
func NewInverseMutationNode(alias, address, verb, method string, params map[string]string) (Node, error) {
	if method == "" {
		return nil, fmt.Errorf("inverse method is required")
	}
	return node{alias: alias, address: address, verb: verb, pin: method, params: params, whole: true}, nil
}
