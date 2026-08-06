package types

import (
	"io"

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
}

func (t *genericType) Name() string {
	return t.typeString
}

func (t *genericType) Equals(other facade.Type) bool {
	return t.Name() == other.Name()
}

func (t *genericType) WriteTo(w io.Writer) (int64, error) {
	n, err := w.Write([]byte(t.typeString))
	return int64(n), err
}

func NewBytesType() facade.Type {
	return &genericType{typeString: typeNameBytes}
}

func NewStringType() facade.Type {
	return &genericType{typeString: typeNameString}
}

func NewDocType() facade.Type {
	return &genericType{typeString: typeNameDoc}
}
