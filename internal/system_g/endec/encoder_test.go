package encoder

import (
	"bytes"
	"testing"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/record"
	"github.com/stackql-labs/omnisdk/internal/system_g/value"
)

// A row is written as every sink writes it: no HTML escaping, no exponent form, keys sorted.
func TestJSONLRendersAsEverySink(t *testing.T) {
	doc := []any{map[string]any{"s": "<script>&", "n": 1e21}}
	var b bytes.Buffer
	if _, err := NewJSONLEncoder().Encode(&b, record.NewRecord(map[string]facade.Value{
		facade.AnonymousPayload: value.NewDocValue(doc),
	})); err != nil {
		t.Fatal(err)
	}
	if want := `{"n":1000000000000000000000,"s":"<script>&"}` + "\n"; b.String() != want {
		t.Errorf("got %q, want %q", b.String(), want)
	}
}
