package types

import (
	"io"

	"github.com/stackql-labs/omnisdk/internal/kind"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

const (
	typeNameBytes  = "bytes"
	typeNameString = "string"
	typeNameDoc    = "doc"
)

var (
	_ facade.Type = &genericType{}
)

type genericType struct {
	typeString string
	format     string
	kind       facade.Kind
}

func (t *genericType) Name() string {
	return t.typeString
}

func (t *genericType) Format() string {
	return t.format
}

func (t *genericType) Kind() facade.Kind {
	return t.kind
}

func (t *genericType) Equals(other facade.Type) bool {
	return t.Name() == other.Name()
}

func (t *genericType) WriteTo(w io.Writer) (int64, error) {
	n, err := w.Write([]byte(t.typeString))
	return int64(n), err
}

func NewBytesType() facade.Type {
	return &genericType{typeString: typeNameBytes, kind: kind.String()}
}

func NewStringType() facade.Type {
	return &genericType{typeString: typeNameString, kind: kind.String()}
}

func NewDocType() facade.Type {
	return &genericType{typeString: typeNameDoc, kind: kind.Object()}
}

// New builds a type as a document states it: its own name, its format refinement, and the kind that
// says what can be done with its values. This is where a provider document lands — nothing else
// invents a type.
func New(name, format string, k facade.Kind) facade.Type {
	if k == nil {
		// A document that states no usable type yields Unknown rather than a default to string:
		// guessing the kind is what puts a rewritten value on the wire.
		k = kind.Unknown()
	}
	return &genericType{typeString: name, format: format, kind: k}
}
