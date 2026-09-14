// Package kind implements the value kinds behind facade.Kind.
//
// Every kind keeps the lexeme it was given and never round-trips a value through a Go numeric type
// in transit. Comparison is defined per kind — exact for integers however long, numeric for
// decimals, case-sensitive for strings — so "has this converged?" is answered in the value's own
// terms rather than by string equality on two float64 approximations.
package kind

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"reflect"
	"strconv"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

// typed is a lexeme that knows its kind.
type typed struct {
	kind   facade.Kind
	lexeme []byte
}

func (t typed) Kind() facade.Kind { return t.kind }

// Reader replays the lexeme. A bounded value may be read as often as wanted, which is what lets it
// be compared at all.
func (t typed) Reader() io.Reader { return bytes.NewReader(t.lexeme) }

// Lexeme hands back a copy: a value that can be edited by its holder is not the value that was
// parsed, and the ledger keeps these.
func (t typed) Lexeme() []byte { return append([]byte(nil), t.lexeme...) }

// String is text. Comparison is byte order; any lexeme is valid.
func String() facade.Kind { return stringKind{} }

type stringKind struct{}

func (k stringKind) Name() string     { return "string" }
func (k stringKind) Comparable() bool { return true }

func (k stringKind) Parse(lexeme []byte) (facade.Typed, error) {
	return typed{kind: k, lexeme: lexeme}, nil
}

func (k stringKind) Compare(a, b facade.Typed) (int, error) {
	return bytes.Compare(a.Lexeme(), b.Lexeme()), nil
}

func (k stringKind) Encode(v facade.Typed) ([]byte, error) { return v.Lexeme(), nil }

// Int is an integer of any width. It is held and compared as an arbitrary-precision integer, so an
// AWS account id or a 64-bit identifier compares exactly rather than through float64.
func Int() facade.Kind { return intKind{} }

type intKind struct{}

func (k intKind) Name() string     { return "int" }
func (k intKind) Comparable() bool { return true }

func (k intKind) Parse(lexeme []byte) (facade.Typed, error) {
	if _, ok := new(big.Int).SetString(string(lexeme), 10); !ok {
		return nil, fmt.Errorf("kind: %q is not an integer", lexeme)
	}
	return typed{kind: k, lexeme: lexeme}, nil
}

func (k intKind) Compare(a, b facade.Typed) (int, error) {
	x, ok := new(big.Int).SetString(string(a.Lexeme()), 10)
	if !ok {
		return 0, fmt.Errorf("kind: %q is not an integer", a.Lexeme())
	}
	y, ok := new(big.Int).SetString(string(b.Lexeme()), 10)
	if !ok {
		return 0, fmt.Errorf("kind: %q is not an integer", b.Lexeme())
	}
	return x.Cmp(y), nil
}

// Encode renders the integer as written. Normalising it would change the bytes a provider signs.
func (k intKind) Encode(v facade.Typed) ([]byte, error) { return v.Lexeme(), nil }

// Decimal is an exact decimal. 1.10 and 1.1 are EQUAL as numbers and DIFFERENT as lexemes; compare
// answers the first, encode preserves the second, which is why both are kept.
func Decimal() facade.Kind { return decimalKind{} }

type decimalKind struct{}

func (k decimalKind) Name() string     { return "decimal" }
func (k decimalKind) Comparable() bool { return true }

func (k decimalKind) Parse(lexeme []byte) (facade.Typed, error) {
	if _, ok := new(big.Rat).SetString(string(lexeme)); !ok {
		return nil, fmt.Errorf("kind: %q is not a decimal", lexeme)
	}
	return typed{kind: k, lexeme: lexeme}, nil
}

func (k decimalKind) Compare(a, b facade.Typed) (int, error) {
	x, ok := new(big.Rat).SetString(string(a.Lexeme()))
	if !ok {
		return 0, fmt.Errorf("kind: %q is not a decimal", a.Lexeme())
	}
	y, ok := new(big.Rat).SetString(string(b.Lexeme()))
	if !ok {
		return 0, fmt.Errorf("kind: %q is not a decimal", b.Lexeme())
	}
	return x.Cmp(y), nil
}

func (k decimalKind) Encode(v facade.Typed) ([]byte, error) { return v.Lexeme(), nil }

// Bool is true or false.
func Bool() facade.Kind { return boolKind{} }

type boolKind struct{}

func (k boolKind) Name() string     { return "bool" }
func (k boolKind) Comparable() bool { return true }

func (k boolKind) Parse(lexeme []byte) (facade.Typed, error) {
	if _, err := strconv.ParseBool(string(lexeme)); err != nil {
		return nil, fmt.Errorf("kind: %q is not a bool", lexeme)
	}
	return typed{kind: k, lexeme: lexeme}, nil
}

func (k boolKind) Compare(a, b facade.Typed) (int, error) {
	x, _ := strconv.ParseBool(string(a.Lexeme()))
	y, _ := strconv.ParseBool(string(b.Lexeme()))
	switch {
	case x == y:
		return 0, nil
	case !x:
		return -1, nil
	default:
		return 1, nil
	}
}

func (k boolKind) Encode(v facade.Typed) ([]byte, error) { return v.Lexeme(), nil }

// Unknown is what a document that states no type yields. It carries the lexeme unchanged and
// refuses to order it: two unknown values can be tested for identity by their lexemes, but claiming
// one is less than the other would be inventing a type the document never gave.
func Unknown() facade.Kind { return unknownKind{} }

type unknownKind struct{}

func (k unknownKind) Name() string     { return "unknown" }
func (k unknownKind) Comparable() bool { return true }

func (k unknownKind) Parse(lexeme []byte) (facade.Typed, error) {
	return typed{kind: k, lexeme: lexeme}, nil
}

func (k unknownKind) Compare(a, b facade.Typed) (int, error) {
	if bytes.Equal(a.Lexeme(), b.Lexeme()) {
		return 0, nil
	}
	return 0, fmt.Errorf("kind: unknown admits no order; %q and %q differ", a.Lexeme(), b.Lexeme())
}

func (k unknownKind) Encode(v facade.Typed) ([]byte, error) { return v.Lexeme(), nil }

// Stream is an unbounded value: bytes from a firehose, a large object body, anything that does not
// fit in memory and may be read only once.
//
// It is deliberately incomparable. Deciding whether such a value has converged would mean reading
// all of it, and there is no all — so a stream never participates in a merge, and convergence over
// one happens against a digest or ETag the provider supplies. Refusing is the honest answer;
// comparing a prefix would be worse than not comparing at all.
func Stream() facade.Kind { return streamKind{} }

type streamKind struct{}

func (k streamKind) Name() string     { return "stream" }
func (k streamKind) Comparable() bool { return false }

// Parse over a slice is a bounded value wearing an unbounded kind, which is a caller error rather
// than something to quietly accept. FromReader is how a stream is made.
func (k streamKind) Parse([]byte) (facade.Typed, error) {
	return nil, fmt.Errorf("kind: stream has no lexeme; build it from a reader")
}

// FromReader wraps a stream as a value of the Stream kind.
func FromReader(r io.Reader) facade.Typed { return stream{r: r} }

type stream struct{ r io.Reader }

func (s stream) Kind() facade.Kind { return streamKind{} }
func (s stream) Reader() io.Reader { return s.r }

// Lexeme is nil: there is no bounded form to hand back, and returning a partial one would be a lie.
func (s stream) Lexeme() []byte { return nil }

func (k streamKind) Compare(facade.Typed, facade.Typed) (int, error) {
	return 0, fmt.Errorf("kind: stream is not comparable; converge against a digest instead")
}

// Encode refuses for the same reason: rendering the value means materialising it, and the caller
// that needs the bytes should read them.
func (k streamKind) Encode(facade.Typed) ([]byte, error) {
	return nil, fmt.Errorf("kind: stream must be read, not encoded")
}

// Object is a document: a set of named fields, held as the bytes it was written as. Comparison is
// structural — field by field, order-insensitive, numbers by value — because two encodings of the
// same object are the same object, and a byte comparison would report drift on a reordered key.
func Object() facade.Kind { return objectKind{} }

type objectKind struct{}

func (k objectKind) Name() string     { return "object" }
func (k objectKind) Comparable() bool { return true }

func (k objectKind) Parse(lexeme []byte) (facade.Typed, error) {
	if _, err := decodeObject(lexeme); err != nil {
		return nil, err
	}
	return typed{kind: k, lexeme: append([]byte(nil), lexeme...)}, nil
}

// Compare reports equality only. An object admits no order — asking which of two documents is
// greater has no answer — so unequal is an error rather than a sign.
func (k objectKind) Compare(a, b facade.Typed) (int, error) {
	x, err := decodeObject(a.Lexeme())
	if err != nil {
		return 0, err
	}
	y, err := decodeObject(b.Lexeme())
	if err != nil {
		return 0, err
	}
	if reflect.DeepEqual(x, y) {
		return 0, nil
	}
	return 0, fmt.Errorf("kind: object admits no order; the two documents differ")
}

func (k objectKind) Encode(v facade.Typed) ([]byte, error) { return v.Lexeme(), nil }

// decodeObject reads a document keeping numbers as their written text, so two objects differing
// only in how a number was spelled are not reported as different.
func decodeObject(b []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var o map[string]any
	if err := dec.Decode(&o); err != nil {
		return nil, fmt.Errorf("kind: not a JSON object: %w", err)
	}
	return o, nil
}
