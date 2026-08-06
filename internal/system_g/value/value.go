package value

import (
	"bytes"
	"encoding/json"
	"io"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/types"
)

var (
	_ facade.Value = bytesValue{}
	_ facade.Value = docValue{}
)

type bytesValue struct {
	data []byte
}

func NewBytesValue(data []byte) facade.Value {
	return bytesValue{data: data}
}

func (v bytesValue) WriteTo(w io.Writer) (int64, error) {
	n, err := w.Write(v.data)
	return int64(n), err
}

func (v bytesValue) Reader() io.Reader {
	return bytes.NewReader(v.data)
}

func (v bytesValue) Type() facade.Type {
	return types.NewBytesType()
}

// docValue carries an encoding-agnostic document tree (e.g. what mxj decodes XML into, or
// what JSON decodes to): plain Go maps/slices/scalars, independent of any wire encoding. Its
// byte form is JSON, but the tree itself is the value; use Doc to recover it.
type docValue struct {
	doc any
}

// NewDocValue wraps an agnostic document tree.
func NewDocValue(doc any) facade.Value {
	return docValue{doc: doc}
}

// Doc recovers the agnostic tree from a value, false if v is not a docValue.
func Doc(v facade.Value) (any, bool) {
	dv, ok := v.(docValue)
	if !ok {
		return nil, false
	}
	return dv.doc, true
}

func (v docValue) WriteTo(w io.Writer) (int64, error) {
	b, err := json.Marshal(v.doc)
	if err != nil {
		return 0, err
	}
	n, err := w.Write(b)
	return int64(n), err
}

func (v docValue) Reader() io.Reader {
	b, err := json.Marshal(v.doc)
	if err != nil {
		return bytes.NewReader(nil)
	}
	return bytes.NewReader(b)
}

func (v docValue) Type() facade.Type {
	return types.NewDocType()
}
