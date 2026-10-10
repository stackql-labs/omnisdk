package sqlfn_test

import (
	"testing"
)

func one(names ...string) []parity {
	out := make([]parity, len(names))
	for i, n := range names {
		out[i] = parity{name: n, arity: []int{1}}
	}
	return out
}

var mathCases = append(one("abs", "acos", "acosh", "asin", "asinh", "atan", "atanh", "ceil", "ceiling",
	"cos", "cosh", "degrees", "exp", "floor", "ln", "log10", "log2", "radians", "sign", "sin", "sinh",
	"sqrt", "tan", "tanh", "trunc", "round"),
	parity{name: "atan2", arity: []int{2}},
	parity{name: "log", arity: []int{1, 2}},
	parity{name: "mod", arity: []int{2}},
	parity{name: "pow", arity: []int{2}},
	parity{name: "power", arity: []int{2}},
	parity{name: "round", arity: []int{2}},
	parity{name: "pi", args: [][]any{{}}},
)

var textCases = append(one("lower", "upper", "length", "octet_length", "hex", "unhex", "quote",
	"soundex", "unicode", "trim", "ltrim", "rtrim", "typeof", "likely", "unlikely", "char", "unistr",
	"unistr_quote", "zeroblob"),
	parity{name: "trim", arity: []int{2}},
	parity{name: "ltrim", arity: []int{2}},
	parity{name: "rtrim", arity: []int{2}},
	parity{name: "instr", arity: []int{2}},
	parity{name: "substr", arity: []int{2, 3}},
	parity{name: "substring", arity: []int{2, 3}},
	parity{name: "replace", arity: []int{3}},
	parity{name: "unhex", arity: []int{2}},
	parity{name: "char", arity: []int{2, 3}},
	parity{name: "concat", arity: []int{1, 2, 3}},
	parity{name: "concat_ws", arity: []int{2, 3}},
	parity{name: "glob", arity: []int{2}},
	parity{name: "like", arity: []int{2, 3}},
)

func TestSQLiteParityMath(t *testing.T) { checkParity(t, sqliteCatalog(t), mathCases) }
func TestSQLiteParityText(t *testing.T) { checkParity(t, sqliteCatalog(t), textCases) }

var textTargeted = []parity{
	{name: "unistr", args: [][]any{{`A\0042`}, {`é`}, {`\+01F600`}, {`\U0001F600`}, {`a\\b`}, {`\x`}, {`\12`}, {`\D800`}, {`\110000`}, {`plain`}}},
	{name: "unistr_quote", args: [][]any{{"a\tb"}, {"a'b"}, {`back\slash`}, {"nl\n"}, {"plain"}, {int64(5)}, {nil}, {[]byte{1, 2}}}},
	{name: "quote", args: [][]any{{"a\x00b"}, {"it's"}, {0.1}, {-0.0}, {1e300}}},
	{name: "char", args: [][]any{{int64(72), int64(105)}, {int64(0x1F600)}, {int64(0x110000)}, {int64(0xD800)}, {"65"}, {66.9}, {nil}, {}}},
	{name: "glob", args: [][]any{{"[abc]*", "beta"}, {"[^abc]*", "beta"}, {"[a-c]x", "bx"}, {"*.go", "main.go"}, {"?", "é"}, {"[]]", "]"}, {"a[", "a["}, {"*", ""}, {"**?", "x"}}},
	{name: "like", args: [][]any{{"%é%", "café"}, {"É", "é"}, {"_", "é"}, {"a%b", "aXYb"}, {"a!%b", "a%b", "!"}, {"a!_", "aX", "!"}, {"!!", "!", "!"}, {"%", ""}, {"%%", "x"}}},
	{name: "substr", args: [][]any{{"héllo", int64(-3), int64(2)}, {"héllo", int64(2), int64(-1)}, {"héllo", int64(10)}, {"abc", int64(-5), int64(3)}, {[]byte("abcdef"), int64(-2)}, {[]byte("abcdef"), int64(3), int64(-2)}}},
	{name: "instr", args: [][]any{{"héllo", "llo"}, {"abc", ""}, {"", ""}, {[]byte("abc"), []byte("c")}, {[]byte("abc"), "c"}, {int64(12345), int64(45)}}},
	{name: "replace", args: [][]any{{"aaa", "aa", "b"}, {"héllo", "é", "e"}, {int64(1212), int64(12), "x"}}},
	{name: "trim", args: [][]any{{"xyxaxy", "xy"}, {"ééaéé", "é"}, {"  a  "}, {"\ta\t"}}},
	{name: "soundex", args: [][]any{{"Tymczak"}, {"Pfister"}, {"Ashcraft"}, {"Honeyman"}, {"lloyd"}, {"A"}, {"-abc"}}},
}

func TestSQLiteParityTextTargeted(t *testing.T) {
	checkParity(t, sqliteCatalog(t), textTargeted)
}

// likelihood's probability must be a constant between 0 and 1, so it cannot be bound: each case is
// written as a literal.
func TestSQLiteParityLikelihood(t *testing.T) {
	var specs []parity
	for _, lit := range []struct {
		sql string
		p   any
	}{{"0.5", 0.5}, {"0.0", 0.0}, {"1.0", 1.0}, {"2.0", 2.0}, {"-0.5", -0.5}, {"1", int64(1)}, {"'x'", "x"}} {
		specs = append(specs, parity{name: "likelihood", sql: "likelihood(?, " + lit.sql + ")"})
		specs[len(specs)-1].args = [][]any{{"a", lit.p}, {nil, lit.p}, {int64(3), lit.p}}
	}
	checkParityLiteral(t, sqliteCatalog(t), specs)
}

var condCases = []parity{
	{name: "coalesce", arity: []int{2, 3}},
	{name: "ifnull", arity: []int{2}},
	{name: "nullif", arity: []int{2}},
	{name: "iif", arity: []int{2, 3}},
	{name: "if", arity: []int{2, 3}},
	{name: "iif", args: [][]any{{int64(0), "a", int64(1), "b"}, {int64(0), "a", int64(0), "b", "c"}, {nil, "a", "1x", "b"}}},
	{name: "max", arity: []int{2, 3}},
	{name: "min", arity: []int{2, 3}},
	{name: "typeof", arity: []int{1}},
	{name: "subtype", arity: []int{1}},
}

func TestSQLiteParityConditional(t *testing.T) {
	checkParity(t, sqliteCatalog(t), condCases)
}
