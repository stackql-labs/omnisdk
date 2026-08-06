package record

import (
	"io"
	"maps"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/value"
)

var (
	_ facade.Record = &record{}
	// _ facade.Records = &recordStream{}
)

// record is the standard map-backed facade.Record. Values are fixed at construction;
// there is no mutator, so a constructed record is immutable by type.
type record struct {
	m map[string]facade.Value
}

func (r *record) Get(k string) facade.Value {
	return r.m[k]
}

func (r *record) Len() int {
	return len(r.m)
}

// Bytes is the Page view of a value: its materialized bytes (nil if absent). The value is
// already in memory, so this neither blocks nor reaches a source.
func (r *record) Bytes(k string) []byte {
	v := r.m[k]
	if v == nil {
		return nil
	}
	b, _ := io.ReadAll(v.Reader())
	return b
}

// Doc is the Page view of a value: its agnostic document tree (false if absent or not a doc).
func (r *record) Doc(k string) (any, bool) {
	v := r.m[k]
	if v == nil {
		return nil, false
	}
	return value.Doc(v)
}

// NewRecord builds an immutable record from the given designation→value map.
// The map is copied so the caller cannot mutate the record after construction.
func NewRecord(m map[string]facade.Value) facade.Record {
	cp := make(map[string]facade.Value, len(m))
	maps.Copy(cp, m)
	return &record{m: cp}
}

// type recordStream struct {
// 	records []facade.Record
// 	index   int
// 	err     error
// }

// func (s *recordStream) Next(_ context.Context) bool {
// 	if s.index < len(s.records) {
// 		s.index++
// 		return true
// 	}
// 	return false
// }

// func (s *recordStream) Record() facade.Record {
// 	if s.index == 0 || s.index > len(s.records) {
// 		panic("Record called before Next or after EOF")
// 	}
// 	return s.records[s.index-1]
// }

// func (s *recordStream) Err() error {
// 	return s.err
// }
