package sqlfn_test

import (
	"strings"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/sqlfn"
)

// The regular expression functions are checked against Postgres over patterns that exercise the
// ARE engine's every construct, each in each function, flag and syntax flavour.

// pgRePatterns are patterns across the engine's constructs, valid and not.
var pgRePatterns = []string{
	// literals and simple atoms
	"", "a", "abc", "b", "x", ".", "..", "a.c", "o", "l+", "hello", "日本", "😀", "é",
	// quantifiers, greedy and not
	"a*", "a+", "a?", "a*?", "a+?", "a??", "a{2}", "a{1,2}", "a{2,}", "a{0}", "a{0,0}", "a{,2}",
	"a{1,2}?", "a{2,}?", "(a|ab)(c|bcd)(d*)", "(a+)(b+)?", "(.*)(\\d+)", "(.*?)(\\d+)",
	"(a*)+", "(a*)*", "(a|b)*c", "(a|b)*?c", "x*y*", "(ab){2,3}", "(?:ab){2,3}?", "a{256}", "a{3,2}",
	"((a)|b)+", "(a)|(b)", "(a)?(b)?c", "(()|a)+", "(.)(.)?",
	// anchors
	"^a", "a$", "^$", "^", "$", "^abc$", "\\Aa", "c\\Z", "^.*$", "(^a|b$)",
	// bracket expressions and classes
	"[abc]", "[^abc]", "[a-c]", "[a-]", "[]a]", "[^]a]", "[[:alpha:]]+", "[[:digit:]]+", "[[:alnum:]]+",
	"[[:space:]]", "[[:upper:]]", "[[:lower:]]+", "[[:punct:]]", "[[:xdigit:]]+", "[[:word:]]+",
	"[[:blank:]]", "[[:cntrl:]]", "[[:graph:]]+", "[[:print:]]+", "[[:ascii:]]+", "[[:bogus:]]",
	"[[.hyphen.]]", "[[.a.]-c]", "[[=a=]]", "[[.nope.]]", "[a-z0-9_]+", "[z-a]", "[\\d]", "[\\w\\s]+",
	"[^\\d]+", "[\\D]", "[\\s\\S]", "[ä-ü]", "[α-ω]+", "[^a-z]+", "[[:<:]]", "[[:<:]]h", "o[[:>:]]",
	"[abc", "[a-c-e]", "[\\]", "[.]", "[^\\n]+",
	// escapes
	"\\d", "\\d+", "\\D+", "\\w+", "\\W", "\\s", "\\S+", "\\y", "\\yhe", "lo\\y", "\\Y", "\\m", "\\mw",
	"\\M", "o\\M", "\\Bx", "\\x41", "\\x{41}", "\\u00e9", "\\U0001F600", "\\t", "\\n", "\\101",
	"\\0", "\\e", "\\a", "\\cA", "\\q", "\\", "\\.", "\\*", "\\(", "\\b", "\\u12",
	// backreferences
	"(a)\\1", "(.)\\1", "(.+)\\1", "(\\w+) \\1", "(a*)\\1", "(.)(.)\\2\\1", "\\1", "(a)\\2", "((a)b)\\2",
	"(.)\\1{2}", "(.)\\1*", "(l)\\1?", "([a-c])x\\1", "(b|c)\\1+",
	// lookaround
	"a(?=b)", "a(?!b)", "(?<=a)b", "(?<!a)b", "\\w+(?=\\s)", "(?<=\\s)\\w+", "(?=.*o)h", "a(?=)",
	"(?<=^|,)[^,]*", "x(?=y", "(?<=a|bc)d", "(?!a).", "(?<=é)l",
	// alternation and grouping
	"a|b", "abc|bcd", "|a", "a|", "(a|ab)c", "(?:a|b)+", "(a)(b)(c)", "((a)(b))", "(?:)", "()", "(a",
	"a)", ")", "(?#comment)a", "(?:a(?:b(?:c)))", "(?x) a b # comment", "(?i)ABC", "(?i)[a-c]+",
	"(?c)abc", "(?q)a.b", "(?e)a{2}", "(?b)a\\{2\\}", "(?n)^b", "(?s).", "(?p)a.b", "(?w)^b", "(?z)a",
	"(?i", "(?ix)  h E l", "(?m)l$",
	// syntax prefixes
	"***=a.b", "***:a+", "***?", "***x", "***:(?i)A",
	// errors and edge cases
	"*", "+a", "a**", "a{", "a{1", "a{1,2", "a{x}", "{1}", "a{1}{2}", "(*)", "[", "]", "?", "a|*",
	"(?<x)", "(?", "(?P<n>a)",
}

// pgReSubjects are strings to match against.
var pgReSubjects = []any{
	"", "a", "abc", "abcabc", "aaa", "aab", "abab", "hello world", "Hello World", "x1y22z333",
	"a,b,,c", "  padded  ", "line1\nline2\nb", "ABC abc", "the the cat", "abcd", "ébé", "日本語テキスト日本",
	"😀x😀", "aaaa bbbb", "xyz", "bcd", "a-b_c", "tab\there", "über naïve", "mississippi",
}

func TestPgParityRegexMatch(t *testing.T) {
	var cases []pgCase
	for _, p := range pgRePatterns {
		for _, s := range pgReSubjects {
			cases = append(cases, pgCase{"regexp_match", []any{s, p}})
		}
	}
	checkPg(t, cases)
}

func TestPgParityRegexSubstring(t *testing.T) {
	var cases []pgCase
	for _, p := range pgRePatterns {
		for _, s := range pgReSubjects {
			cases = append(cases, pgCase{"substring", []any{s, p}})
		}
	}
	cases = append(cases, pgCase{"substring", []any{nil, "a"}}, pgCase{"substring", []any{"a", nil}},
		pgCase{"substring", []any{"abc", sqlfn.PgUnknown("b.")}})
	checkPg(t, cases)
}

// pgReFlagSets are flag arguments: every letter, combinations, and invalid ones.
var pgReFlagSets = []any{
	"", "g", "i", "c", "n", "m", "p", "w", "s", "x", "t", "b", "e", "q", "gi", "ig", "in", "ix", "nx",
	"qi", "qx", "qn", "bi", "ei", "be", "eb", "ic", "ci", "np", "pw", "z", "gz", "é", "G", "ng", "sg",
	"xi", "bx", "qe", "qb",
}

func TestPgParityRegexFlags(t *testing.T) {
	patterns := []string{"a", "^b", "b$", "A.C", "a.b", "a b", "(?x)a b", "[^a]", ".", "l+", "^l", "e$",
		"a{2}", "a\\{2\\}", "\\(a\\)", "(a)\\1", "a|b", "a+?", "\\w+", "[[:upper:]]+", "a(?=b)", "\\d",
		"hello # c", "ab|c", "a\\.b", "[[:lower:]]", "(?i)a", "a\\nb"}
	subjects := []any{"", "aAbB", "a.b\nb", "ABC\nabc", "line1\nline2\nb", "a b ab", "aa a{2}",
		"(a)aa", "Hello World", "xa\nb", "hello # c"}
	var cases []pgCase
	for _, f := range pgReFlagSets {
		for _, p := range patterns {
			for _, s := range subjects {
				cases = append(cases, pgCase{"regexp_match", []any{s, p, f}},
					pgCase{"regexp_matches", []any{s, p, f}},
					pgCase{"regexp_replace", []any{s, p, "<\\&>", f}},
					pgCase{"regexp_split_to_array", []any{s, p, f}})
			}
		}
	}
	checkPg(t, cases)
}

func TestPgParityRegexMatches(t *testing.T) {
	var cases []pgCase
	for _, p := range pgRePatterns {
		for _, s := range pgReSubjects {
			cases = append(cases, pgCase{"regexp_matches", []any{s, p}},
				pgCase{"regexp_matches", []any{s, p, "g"}})
		}
	}
	cases = append(cases, pgCase{"regexp_matches", []any{nil, "a"}}, pgCase{"regexp_matches", []any{"a", nil}},
		pgCase{"regexp_matches", []any{"a", "a", nil}})
	checkPg(t, cases)
}

func TestPgParityRegexReplace(t *testing.T) {
	replacements := []string{"", "X", "<\\&>", "[\\1]", "\\2\\1", "\\\\", "\\", "a\\b", "\\0", "\\9",
		"é\\1é", "\\&\\&", "x\\", "\\\\1"}
	patterns := []string{"a", "(a)", "(.)(.)", "", "b*", "(l+)", "o", "(\\w+) (\\w+)", "^", "$", "x*",
		"(é)", ".", "(a)|(b)", "\\y", "(?<=a)", "[[:space:]]+", "(a)(b)?", "(.)\\1"}
	subjects := []any{"", "abc", "aaa", "hello world", "ébé", "日本語", "abab", "a b  c", "mississippi"}
	var cases []pgCase
	for _, r := range replacements {
		for _, p := range patterns {
			for _, s := range subjects {
				cases = append(cases, pgCase{"regexp_replace", []any{s, p, r}},
					pgCase{"regexp_replace", []any{s, p, r, "g"}})
			}
		}
	}
	for _, p := range pgRePatterns {
		for _, s := range pgReSubjects {
			cases = append(cases, pgCase{"regexp_replace", []any{s, p, "<\\&|\\1>", "g"}})
		}
	}
	cases = append(cases, pgCase{"regexp_replace", []any{nil, "a", "b"}}, pgCase{"regexp_replace", []any{"a", nil, "b"}},
		pgCase{"regexp_replace", []any{"a", "a", nil}}, pgCase{"regexp_replace", []any{"a", "a", "b", nil}},
		pgCase{"regexp_replace", []any{"aaa", "a", "b", "gi"}}, pgCase{"regexp_replace", []any{"aAa", "a", "b", "gi"}},
		pgCase{"regexp_replace", []any{"aaa", "a", "b", "y"}})
	checkPg(t, cases)
}

func TestPgParityRegexSplit(t *testing.T) {
	patterns := []string{"", ",", ",*", "\\s+", "\\s*", "a", "b*", "(,)", "x", "[aeiou]", "\\y", "^", "$",
		".", "(?=b)", "\\m", "s+", "日", "é", "[", "😀"}
	var cases []pgCase
	for _, p := range patterns {
		for _, s := range pgReSubjects {
			cases = append(cases, pgCase{"regexp_split_to_table", []any{s, p}},
				pgCase{"regexp_split_to_array", []any{s, p}})
			for _, f := range []any{"i", "g", "x", "q", "z"} {
				cases = append(cases, pgCase{"regexp_split_to_table", []any{s, p, f}},
					pgCase{"regexp_split_to_array", []any{s, p, f}})
			}
		}
	}
	cases = append(cases, pgCase{"regexp_split_to_array", []any{nil, ","}}, pgCase{"regexp_split_to_array", []any{"a,b", nil}},
		pgCase{"regexp_split_to_table", []any{nil, ","}}, pgCase{"regexp_split_to_array", []any{"a,b", ",", nil}})
	checkPg(t, cases)
}

func TestPgParityRegexSimilar(t *testing.T) {
	patterns := []string{"%", "a%", "%b%", "_b_", "a\"#\"%", "%#\"b#\"%", "#\"%#\"", "a|b", "(a|b)%", "[ab]%",
		"%[^a]", "a.b", "a^b$", "%\\d%", "#%", "a#_", "#\"a#\"#\"b#\"", "%#\"#\"%", "%(b)%", "é%", "%日%",
		"[%]", "a*", "a+b", "", "#", "a{2}%", "%\\"}
	escapes := []any{"#", "", "\\", "é", "ab", "日"}
	subjects := []any{"", "abc", "a.b", "b", "aab", "a%b", "é日", "a_b", "a\\1b", "ab"}
	var cases []pgCase
	for _, p := range patterns {
		for _, e := range escapes {
			for _, s := range subjects {
				cases = append(cases, pgCase{"substring", []any{s, p, e}})
			}
		}
	}
	cases = append(cases, pgCase{"substring", []any{"abc", "a%", nil}}, pgCase{"substring", []any{nil, "a%", "#"}})
	checkPg(t, cases)
}

// TestPgParityRegexBig runs patterns whose NFAs and colormaps grow large: long alternations, wide
// ranges beyond the simple-chr cutoff, and long subjects that churn the DFA state cache.
func TestPgParityRegexBig(t *testing.T) {
	var alts []string
	for i := 0; i < 60; i++ {
		alts = append(alts, strings.Repeat(string(rune('a'+i%26)), 1+i%4))
	}
	long := strings.Repeat("abcdefghij", 50) + "日本" + strings.Repeat("xyz", 40)
	patterns := []string{
		strings.Join(alts, "|"), "(" + strings.Join(alts, "|") + ")+", "[ࠀ-￿]+", "[^Ā-Ȁ]+",
		"[߰-ࠐa-c]+", "(?i)[Ѐ-ѐ]+", "(a|b|c|d|e|f|g|h|i|j)*x", "((a|b)(c|d))*",
		"(.)(.)(.)(.)(.)(.)(.)(.)(.)(.)(.)", "(\\w)(\\w)(\\w)(\\w)(\\w)(\\w)(\\w)(\\w)(\\w)(\\w)(\\w)\\11",
		"[a-j]{10}x", "(abc|abcdefghij){3,5}", "(?:[a-z]{2,3}){4,}?y", "(.*)(.*)(.*)z", "a{100,200}",
		strings.Repeat("(a)", 30), "(x+x+)+y", "[[:alpha:]日本]+",
	}
	subjects := []any{long, "", "abcdefghijabcdefghijx", "Привет мир", "ࠀࠁ￯", "aaaa" + strings.Repeat("a", 150),
		strings.Repeat("x", 30), strings.Repeat("ab", 40) + "y"}
	var cases []pgCase
	for _, p := range patterns {
		for _, s := range subjects {
			cases = append(cases, pgCase{"regexp_match", []any{s, p}}, pgCase{"regexp_matches", []any{s, p, "g"}},
				pgCase{"regexp_replace", []any{s, p, "[\\&]", "g"}})
		}
	}
	// Postgres's compile-space limit: the first of each pair compiles, the second is too complex.
	for _, p := range []string{"((a{1,50}){1,50}){1,34}", "((a{1,50}){1,50}){1,35}", "(a{1,255}){1,255}",
		"((((a{1,100}){1,100}){1,100}){1,100})", "((.{1,30}){1,30}){1,30}", "([ab]{1,99}){1,99}"} {
		cases = append(cases, pgCase{"regexp_match", []any{"aaa", p}})
	}
	checkPg(t, cases)
}

// TestPgParityRegexRandom compares patterns assembled at random from the engine's tokens, which
// reach combinations no hand-written list does: nested quantifiers under captures, backrefs into
// alternations, constraints next to each other.
func TestPgParityRegexRandom(t *testing.T) {
	tokens := []string{"a", "b", "c", "é", "日", ".", "\\d", "\\w", "\\s", "\\W", "[ab]", "[^a]", "[a-c]",
		"[[:alpha:]]", "^", "$", "\\y", "\\m", "\\M", "\\A", "\\Z", "*", "+", "?", "*?", "+?", "??", "{2}",
		"{1,3}", "{0,2}?", "{2,}", "|", "(", ")", "(?:", "(?=", "(?!", "(?<=", "(?<!", "\\1", "\\2", " "}
	subjects := []any{"", "abc", "aabbcc", "abcabc", "ab ab", "a日é c", "ccba", "babab", "a\nb", "日日é",
		strings.Repeat("abcab cba日é\n", 30), strings.Repeat("a b c ", 40) + "日"}
	x := uint64(0x2545F4914F6CDD1D)
	rnd := func(n int) int {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		return int(x % uint64(n))
	}
	var cases []pgCase
	for i := 0; i < 3000; i++ {
		var b strings.Builder
		for n := 1 + rnd(12); n > 0; n-- {
			b.WriteString(tokens[rnd(len(tokens))])
		}
		p := b.String()
		for j := 0; j < 2; j++ {
			s := subjects[rnd(len(subjects))]
			switch rnd(4) {
			case 0:
				cases = append(cases, pgCase{"regexp_match", []any{s, p}})
			case 1:
				cases = append(cases, pgCase{"regexp_matches", []any{s, p, "g"}})
			case 2:
				cases = append(cases, pgCase{"regexp_replace", []any{s, p, "<\\&\\1>", "g"}})
			default:
				cases = append(cases, pgCase{"regexp_match", []any{s, p, "i"}})
			}
		}
	}
	checkPg(t, cases)
}
