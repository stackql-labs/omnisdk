package kind_test

import (
	"io"
	"strings"
	"testing"

	"github.com/stackql-labs/omnisdk/internal/kind"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

func parse(t *testing.T, k facade.Kind, lexeme string) facade.Typed {
	t.Helper()
	v, err := k.Parse([]byte(lexeme))
	if err != nil {
		t.Fatalf("parse %q as %s: %v", lexeme, k.Name(), err)
	}
	return v
}

// An integer compares exactly however long it is. Through float64 these two are the same number,
// which is how a drifted resource reads as converged.
func TestIntComparesBeyondFloat64(t *testing.T) {
	k := kind.Int()
	a := parse(t, k, "10000000000000001")
	b := parse(t, k, "10000000000000000")

	if c, err := k.Compare(a, b); err != nil || c <= 0 {
		t.Errorf("compare = %d (%v), want a > b", c, err)
	}
}

// A decimal is equal by value and preserved by lexeme: 1.10 is 1.1, and still encodes as 1.10,
// because normalising changes the bytes a provider signs.
func TestDecimalEqualByValuePreservedByLexeme(t *testing.T) {
	k := kind.Decimal()
	a := parse(t, k, "1.10")
	b := parse(t, k, "1.1")

	if c, err := k.Compare(a, b); err != nil || c != 0 {
		t.Errorf("compare = %d (%v), want equal", c, err)
	}
	enc, err := k.Encode(a)
	if err != nil || string(enc) != "1.10" {
		t.Errorf("encode = %q (%v), want 1.10 unchanged", enc, err)
	}
}

// A lexeme the kind cannot represent is rejected rather than coerced.
func TestParseRejectsWrongLexemes(t *testing.T) {
	for _, tc := range []struct {
		kind   facade.Kind
		lexeme string
	}{
		{kind.Int(), "1.5"},
		{kind.Int(), "abc"},
		{kind.Decimal(), "abc"},
		{kind.Bool(), "yes"},
	} {
		if _, err := tc.kind.Parse([]byte(tc.lexeme)); err == nil {
			t.Errorf("%s accepted %q, want rejection", tc.kind.Name(), tc.lexeme)
		}
	}
}

// Unknown is a real kind, not a default to string: it carries the lexeme and refuses to order two
// different ones, because claiming an order would invent a type the document never gave.
func TestUnknownRefusesOrder(t *testing.T) {
	k := kind.Unknown()
	a := parse(t, k, "10")
	b := parse(t, k, "9")

	if c, err := k.Compare(a, a); err != nil || c != 0 {
		t.Errorf("compare with itself = %d (%v), want equal", c, err)
	}
	if _, err := k.Compare(a, b); err == nil {
		t.Error("unknown ordered two different lexemes, want a refusal")
	}
}

// A holder must not be able to edit the value it was given.
func TestLexemeDoesNotAlias(t *testing.T) {
	v := parse(t, kind.String(), "abc")
	v.Lexeme()[0] = 'X'

	if string(v.Lexeme()) != "abc" {
		t.Errorf("lexeme = %q after caller mutation, want abc", v.Lexeme())
	}
}

// A stream is incomparable by construction: deciding whether it converged would mean reading all of
// it, and there is no all. Refusing is the honest answer — comparing a prefix would be worse.
func TestStreamIsIncomparable(t *testing.T) {
	k := kind.Stream()
	if k.Comparable() {
		t.Error("stream reports comparable")
	}
	v := kind.FromReader(strings.NewReader("firehose"))
	if _, err := k.Compare(v, v); err == nil {
		t.Error("stream compared, want a refusal")
	}
	if _, err := k.Encode(v); err == nil {
		t.Error("stream encoded, want a refusal")
	}
	if v.Lexeme() != nil {
		t.Error("stream handed back a lexeme")
	}
	b, err := io.ReadAll(v.Reader())
	if err != nil || string(b) != "firehose" {
		t.Errorf("reader gave %q (%v), want the stream's bytes", b, err)
	}
}

// Every bounded kind reports comparable, which is what admits it to a merge.
func TestBoundedKindsAreComparable(t *testing.T) {
	for _, k := range []facade.Kind{kind.String(), kind.Int(), kind.Decimal(), kind.Bool(), kind.Unknown()} {
		if !k.Comparable() {
			t.Errorf("%s reports incomparable", k.Name())
		}
	}
}
