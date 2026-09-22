package fn

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/stackql-labs/omnisdk/internal/kind"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

var (
	_ facade.Fn = splitPart{}
	_ facade.Fn = stringToTable{}
)

// splitPart is PostgreSQL's split_part(string, delimiter, n): the n'th field counting from one, or
// from the end when n is negative. Past either end is the empty string, not an error.
type splitPart struct{}

// NewSplitPart builds split_part.
func NewSplitPart() facade.Fn { return splitPart{} }

func (splitPart) Name() string { return "split_part" }

func (splitPart) Signatures() []facade.Signature {
	return []facade.Signature{
		NewSignature(kind.String(), kind.String(), kind.String(), kind.Int()),
	}
}

func (splitPart) Call(_ int, args []facade.Typed) (any, error) {
	if isNull(args...) {
		return nil, nil
	}
	n, err := position(args[2])
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, fmt.Errorf("fn: split_part field position must not be zero")
	}
	parts := fields(text(args[0]), text(args[1]))
	if n < 0 {
		n = len(parts) + n + 1
	}
	if n < 1 || n > len(parts) {
		return "", nil
	}
	return parts[n-1], nil
}

// stringToTable is PostgreSQL's string_to_table(string, delimiter [, null_string]): one row per
// field. The optional third argument is a second signature, not a flag.
type stringToTable struct{ out string }

// NewStringToTable builds string_to_table emitting a single column named out.
func NewStringToTable(out string) facade.Fn { return stringToTable{out: out} }

func (stringToTable) Name() string { return "string_to_table" }

func (f stringToTable) Signatures() []facade.Signature {
	cols := []facade.FnColumn{NewColumn(f.out, kind.String())}
	return []facade.Signature{
		NewRowSignature(cols, kind.String(), kind.String()),
		NewRowSignature(cols, kind.String(), kind.String(), kind.String()),
	}
}

func (f stringToTable) Call(sig int, args []facade.Typed) (any, error) {
	if isNull(args[0], args[1]) {
		return nil, nil
	}
	// Signature 1 declares null_string: a field equal to it is NULL rather than text.
	null, hasNull := "", false
	if sig == 1 && args[2] != nil {
		null, hasNull = text(args[2]), true
	}
	parts := fields(text(args[0]), text(args[1]))
	rows := make([]any, 0, len(parts))
	for _, p := range parts {
		var v any = p
		if hasNull && p == null {
			v = nil
		}
		rows = append(rows, map[string]any{f.out: v})
	}
	return rows, nil
}

// fields cuts s at delim. An empty delimiter yields one field (PostgreSQL); strings.Split would
// instead yield one per character.
func fields(s, delim string) []string {
	if delim == "" {
		return []string{s}
	}
	return strings.Split(s, delim)
}

// isNull reports whether any argument is NULL. These functions are null-propagating.
func isNull(args ...facade.Typed) bool {
	for _, a := range args {
		if a == nil {
			return true
		}
	}
	return false
}

// text is the written form of an argument already parsed as a string.
func text(v facade.Typed) string { return string(v.Lexeme()) }

// position reads an int argument. The int kind is arbitrary-width, so an out-of-range position is
// rejected rather than wrapped.
func position(v facade.Typed) (int, error) {
	n, err := strconv.Atoi(string(v.Lexeme()))
	if err != nil {
		return 0, fmt.Errorf("fn: split_part field position %q is out of range", v.Lexeme())
	}
	return n, nil
}
