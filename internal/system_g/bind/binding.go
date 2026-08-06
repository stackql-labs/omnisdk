package bind

import (
	"fmt"
	"io"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

var (
	_ facade.Beta = binding{}
	_ Binding     = binding{}
)

// Binding is a β edge (paper §β): β ⊆ A^out × A^raw. It names a producer output slot (Src)
// and a consumer inbox slot (Tgt) and carries NO transform — pure identity value-transfer.
// It is the semantic source of truth for cross-exchange composition; the bind join reads
// Src/Tgt to move the value, so the binding is load-bearing, not decorative.
type Binding interface {
	facade.Beta
	Src() string
	Tgt() string
}

type binding struct {
	src string
	tgt string
}

// NewBinding wires the producer attribute src into the consumer slot tgt (identity transfer).
func NewBinding(src, tgt string) Binding {
	return binding{src: src, tgt: tgt}
}

func (b binding) Src() string { return b.src }
func (b binding) Tgt() string { return b.tgt }

func (b binding) WriteTo(w io.Writer) (int64, error) {
	n, err := fmt.Fprintf(w, "beta(%s→%s)", b.src, b.tgt)
	return int64(n), err
}

func (b binding) Publish(w io.Writer) { _, _ = b.WriteTo(w) }
