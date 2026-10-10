package sqlfn

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// SQLite's core text functions (func.c), as the SQLite build stackql embeds has them: no ICU, so
// case is ASCII only.

func sqliteText() []Func {
	return []Func{
		NewScalar("lower", 1, 1, nullable1(func(v any) (any, error) { return asciiCase(sqlText(v), false), nil })),
		NewScalar("upper", 1, 1, nullable1(func(v any) (any, error) { return asciiCase(sqlText(v), true), nil })),
		NewScalar("length", 1, 1, nullable1(func(v any) (any, error) {
			if c, x := norm(v); c == classBlob {
				return int64(len(x.([]byte))), nil
			}
			return int64(utf8.RuneCountInString(beforeNUL(sqlText(v)))), nil
		})),
		NewScalar("octet_length", 1, 1, nullable1(func(v any) (any, error) { return int64(len(sqlBytes(v))), nil })),
		NewScalar("hex", 1, 1, func(a []any) (any, error) {
			if c, _ := norm(a[0]); c == classNull {
				return "", nil
			}
			return fmt.Sprintf("%X", sqlBytes(a[0])), nil
		}),
		NewScalar("unhex", 1, 2, unhexFn),
		NewScalar("quote", 1, 1, func(a []any) (any, error) { return quoteValue(a[0], false), nil }),
		NewScalar("unistr_quote", 1, 1, func(a []any) (any, error) { return quoteValue(a[0], true), nil }),
		NewScalar("unistr", 1, 1, nullable1(func(v any) (any, error) { return unistr(sqlText(v)) })),
		NewScalar("soundex", 1, 1, func(a []any) (any, error) { return soundex(beforeNUL(sqlText(a[0]))), nil }),
		NewScalar("unicode", 1, 1, nullable1(func(v any) (any, error) {
			s := beforeNUL(sqlText(v))
			if s == "" {
				return nil, nil
			}
			r, _ := utf8.DecodeRuneInString(s)
			return int64(r), nil
		})),
		NewScalar("char", 0, -1, func(a []any) (any, error) {
			var b strings.Builder
			for _, v := range a {
				cp := sqlInt(v)
				if cp < 0 || cp > 0x10FFFF {
					cp = 0xFFFD
				}
				b.Write(encodeCodePoint(cp))
			}
			return b.String(), nil
		}),
		NewScalar("trim", 1, 2, trimFn(true, true)),
		NewScalar("ltrim", 1, 2, trimFn(true, false)),
		NewScalar("rtrim", 1, 2, trimFn(false, true)),
		NewScalar("typeof", 1, 1, func(a []any) (any, error) {
			c, _ := norm(a[0])
			return [...]string{"null", "integer", "real", "text", "blob"}[c], nil
		}),
		NewScalar("likely", 1, 1, func(a []any) (any, error) { return a[0], nil }),
		NewScalar("unlikely", 1, 1, func(a []any) (any, error) { return a[0], nil }),
		NewScalar("likelihood", 2, 2, func(a []any) (any, error) {
			c, x := norm(a[1])
			if c != classReal || x.(float64) < 0 || x.(float64) > 1 {
				return nil, errors.New("second argument to likelihood() must be a constant between 0.0 and 1.0")
			}
			return a[0], nil
		}),
		NewScalar("zeroblob", 1, 1, func(a []any) (any, error) {
			n := sqlInt(a[0])
			if n < 0 {
				n = 0
			}
			return make([]byte, n), nil
		}),
		NewScalar("instr", 2, 2, instrFn),
		NewScalar("substr", 2, 3, substrFn),
		NewScalar("substring", 2, 3, substrFn),
		NewScalar("replace", 3, 3, func(a []any) (any, error) {
			if isNull(a[0]) || isNull(a[1]) {
				return nil, nil
			}
			from := sqlText(a[1])
			if from == "" {
				return sqlText(a[0]), nil
			}
			if isNull(a[2]) {
				return nil, nil
			}
			return strings.ReplaceAll(sqlText(a[0]), from, sqlText(a[2])), nil
		}),
		NewScalar("concat", 1, -1, func(a []any) (any, error) {
			var b strings.Builder
			for _, v := range a {
				b.WriteString(sqlText(v))
			}
			return b.String(), nil
		}),
		NewScalar("concat_ws", 2, -1, func(a []any) (any, error) {
			if c, _ := norm(a[0]); c == classNull {
				return nil, nil
			}
			sep := sqlText(a[0])
			var parts []string
			for _, v := range a[1:] {
				if c, _ := norm(v); c != classNull {
					parts = append(parts, sqlText(v))
				}
			}
			return strings.Join(parts, sep), nil
		}),
		NewScalar("glob", 2, 2, func(a []any) (any, error) {
			if isBlob(a[0]) || isBlob(a[1]) {
				return int64(0), nil
			}
			if isNull(a[0]) || isNull(a[1]) {
				return nil, nil
			}
			return globMatch(sqlText(a[0]), sqlText(a[1])), nil
		}),
		NewScalar("like", 2, 3, func(a []any) (any, error) {
			esc := rune(0)
			if len(a) == 3 && !isNull(a[2]) {
				e := []rune(beforeNUL(sqlText(a[2])))
				if len(e) != 1 {
					return nil, errors.New("ESCAPE expression must be a single character")
				}
				esc = e[0]
			}
			// LIKE and GLOB do not match BLOBs: either operand being one is no match, NULL or not.
			if isBlob(a[0]) || isBlob(a[1]) {
				return int64(0), nil
			}
			if isNull(a[0]) || isNull(a[1]) || (len(a) == 3 && isNull(a[2])) {
				return nil, nil
			}
			return likeMatch(sqlText(a[0]), sqlText(a[1]), esc), nil
		}),
	}
}

func isNull(v any) bool { c, _ := norm(v); return c == classNull }
func isBlob(v any) bool { c, _ := norm(v); return c == classBlob }

// nullable1 is a one-argument function NULL for a NULL argument.
func nullable1(f func(any) (any, error)) func([]any) (any, error) {
	return func(a []any) (any, error) {
		if isNull(a[0]) {
			return nil, nil
		}
		return f(a[0])
	}
}

func asciiCase(s string, upper bool) string {
	b := []byte(s)
	for i, c := range b {
		switch {
		case upper && c >= 'a' && c <= 'z':
			b[i] = c - 'a' + 'A'
		case !upper && c >= 'A' && c <= 'Z':
			b[i] = c - 'A' + 'a'
		}
	}
	return string(b)
}

// beforeNUL is text up to its first NUL: SQLite's text functions measure a C string.
func beforeNUL(s string) string {
	if i := strings.IndexByte(s, 0); i >= 0 {
		return s[:i]
	}
	return s
}

func quoteValue(v any, uni bool) string {
	c, x := norm(v)
	switch c {
	case classNull:
		return "NULL"
	case classInteger, classReal:
		return sqlText(x)
	case classBlob:
		return fmt.Sprintf("X'%X'", x.([]byte))
	}
	s := beforeNUL(x.(string))
	quoted := "'" + strings.ReplaceAll(s, "'", "''") + "'"
	if !uni {
		return quoted
	}
	// Only a control character calls for unistr(); once it is used, a backslash must be escaped too.
	needs := false
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			needs = true
			break
		}
	}
	if !needs {
		return quoted
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u%04x`, r)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\'':
			b.WriteString("''")
		default:
			b.WriteRune(r)
		}
	}
	return "unistr('" + b.String() + "')"
}

func unhexFn(a []any) (any, error) {
	if isNull(a[0]) {
		return nil, nil
	}
	ignore := ""
	if len(a) == 2 {
		if isNull(a[1]) {
			return nil, nil
		}
		ignore = sqlText(a[1])
	}
	s := beforeNUL(sqlText(a[0]))
	var out []byte
	hexv := func(c byte) (byte, bool) {
		switch {
		case c >= '0' && c <= '9':
			return c - '0', true
		case c >= 'a' && c <= 'f':
			return c - 'a' + 10, true
		case c >= 'A' && c <= 'F':
			return c - 'A' + 10, true
		}
		return 0, false
	}
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if _, ok := hexv(s[i]); !ok {
			if strings.ContainsRune(ignore, r) {
				i += size
				continue
			}
			return nil, nil
		}
		if i+1 >= len(s) {
			return nil, nil
		}
		hi, _ := hexv(s[i])
		lo, ok := hexv(s[i+1])
		if !ok {
			return nil, nil
		}
		out = append(out, hi<<4|lo)
		i += 2
	}
	if out == nil {
		out = []byte{}
	}
	return out, nil
}

// unistr reads SQLite's unistr() escapes: \XXXX, \uXXXX, \+XXXXXX and \UXXXXXXXX, and \\ for a
// backslash. Any other backslash is an error.
func unistr(s string) (any, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			b.WriteByte(s[i])
			continue
		}
		rest := s[i+1:]
		width, skip := 0, 0
		switch {
		case strings.HasPrefix(rest, `\`):
			b.WriteByte('\\')
			i++
			continue
		case strings.HasPrefix(rest, "u"):
			width, skip = 4, 1
		case strings.HasPrefix(rest, "+"):
			width, skip = 6, 1
		case strings.HasPrefix(rest, "U"):
			width, skip = 8, 1
		default:
			width = 4
		}
		if len(rest) < skip+width {
			return nil, errors.New("invalid Unicode escape")
		}
		cp, err := strconv.ParseUint(rest[skip:skip+width], 16, 32)
		if err != nil || cp > 0x10FFFF {
			return nil, errors.New("invalid Unicode escape")
		}
		b.Write(encodeCodePoint(int64(cp)))
		i += skip + width
	}
	return b.String(), nil
}

// soundex is SQLite's soundex(): the first letter, then the codes of the letters after it, dropping
// repeats of the same code, padded to four characters; "?000" where there is no letter.
func soundex(s string) string {
	codes := map[byte]byte{}
	for c, g := range map[string]byte{"bfpv": '1', "cgjkqsxz": '2', "dt": '3', "l": '4', "mn": '5', "r": '6'} {
		for i := range c {
			codes[c[i]] = g
		}
	}
	i := 0
	isAlpha := func(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }
	for i < len(s) && !isAlpha(s[i]) {
		i++
	}
	if i == len(s) {
		return "?000"
	}
	// A byte is coded by its low seven bits, as SQLite's table lookup does: the bytes of a non-ASCII
	// character code as the ASCII characters they mask to.
	lower := func(c byte) byte {
		c &= 0x7f
		if c >= 'A' && c <= 'Z' {
			return c - 'A' + 'a'
		}
		return c
	}
	out := []byte{asciiCase(s[i:i+1], true)[0]}
	prev := codes[lower(s[i])]
	for i++; i < len(s) && len(out) < 4; i++ {
		code := codes[lower(s[i])]
		if code != 0 {
			if code != prev {
				out = append(out, code)
			}
			prev = code
		} else {
			prev = 0
		}
	}
	for len(out) < 4 {
		out = append(out, '0')
	}
	return string(out)
}

// trimFn is trim, ltrim and rtrim: every character of the set (default a space) removed from the
// chosen ends.
func trimFn(left, right bool) func([]any) (any, error) {
	return func(a []any) (any, error) {
		if isNull(a[0]) {
			return nil, nil
		}
		set := " "
		if len(a) == 2 {
			if isNull(a[1]) {
				return nil, nil
			}
			// The set is read as a C string: it ends at its first NUL.
			set = beforeNUL(sqlText(a[1]))
		}
		s := sqlText(a[0])
		if left {
			s = strings.TrimLeft(s, set)
		}
		if right {
			s = strings.TrimRight(s, set)
		}
		return s, nil
	}
}

// instrFn is instr(H, N): the 1-based position of N in H, 0 where absent — in bytes where both are
// BLOBs, in characters otherwise.
func instrFn(a []any) (any, error) {
	if isNull(a[0]) || isNull(a[1]) {
		return nil, nil
	}
	hc, h := norm(a[0])
	nc, n := norm(a[1])
	if hc == classBlob && nc == classBlob {
		i := strings.Index(string(h.([]byte)), string(n.([]byte)))
		return int64(i + 1), nil
	}
	hs, ns := sqlText(h), sqlText(n)
	i := strings.Index(hs, ns)
	if i < 0 {
		return int64(0), nil
	}
	return int64(utf8.RuneCountInString(hs[:i]) + 1), nil
}

// substrFn is SQLite's substrFunc: characters of text, walked as a C string; bytes of a BLOB.
// Y is 1-based, 0 and negatives counting as SQLite counts them; Z defaults to SQLite's length limit
// and, negative, takes the characters before Y. The arithmetic is SQLite's int64 arithmetic,
// wrapping included.
func substrFn(a []any) (any, error) {
	for _, v := range a {
		if isNull(v) {
			return nil, nil
		}
	}
	const lengthLimit = 1000000000
	c, x := norm(a[0])
	p1 := sqlInt(a[1])
	var blob []byte
	var runes []rune
	var length int64
	if c == classBlob {
		blob = x.([]byte)
		length = int64(len(blob))
	} else {
		runes = []rune(beforeNUL(sqlText(x)))
		if p1 < 0 {
			length = int64(len(runes))
		}
	}
	var p2 int64
	negP2 := false
	if len(a) == 3 {
		p2 = sqlInt(a[2])
		if p2 < 0 {
			p2 = -p2
			negP2 = true
		}
	} else {
		p2 = lengthLimit
	}
	if p1 < 0 {
		p1 += length
		if p1 < 0 {
			if p2 < 0 {
				p2 = 0
			} else {
				p2 += p1
			}
			p1 = 0
		}
	} else if p1 > 0 {
		p1--
	} else if p2 > 0 {
		p2--
	}
	if negP2 {
		p1 -= p2
		if p1 < 0 {
			p2 += p1
			p1 = 0
		}
	}
	if p2 < 0 {
		p2 = 0
	}
	if c == classBlob {
		if p1 >= length {
			return []byte{}, nil
		}
		if p2 > length-p1 {
			p2 = length - p1
		}
		return append([]byte{}, blob[p1:p1+p2]...), nil
	}
	n := int64(len(runes))
	if p1 > n {
		p1 = n
	}
	end := n
	if p2 < n-p1 {
		end = p1 + p2
	}
	return string(runes[p1:end]), nil
}

// globMatch is SQLite's GLOB: case-sensitive, * any run, ? any one character, [...] a set
// (^ negating, - ranges).
func globMatch(pattern, s string) bool {
	p, t := []rune(pattern), []rune(s)
	var match func(i, j int) bool
	match = func(i, j int) bool {
		for i < len(p) {
			switch p[i] {
			case '*':
				for i < len(p) && (p[i] == '*' || p[i] == '?') {
					if p[i] == '?' {
						if j >= len(t) {
							return false
						}
						j++
					}
					i++
				}
				if i == len(p) {
					return true
				}
				for k := j; k <= len(t); k++ {
					if match(i, k) {
						return true
					}
				}
				return false
			case '?':
				if j >= len(t) {
					return false
				}
				i, j = i+1, j+1
			case '[':
				if j >= len(t) {
					return false
				}
				end := i + 1
				invert := false
				if end < len(p) && p[end] == '^' {
					invert = true
					end++
				}
				start := end
				if end < len(p) && p[end] == ']' {
					end++
				}
				for end < len(p) && p[end] != ']' {
					end++
				}
				if end >= len(p) {
					return false
				}
				set := p[start:end]
				found := false
				for k := 0; k < len(set); k++ {
					if k+2 < len(set) && set[k+1] == '-' {
						if t[j] >= set[k] && t[j] <= set[k+2] {
							found = true
						}
						k += 2
						continue
					}
					if set[k] == t[j] {
						found = true
					}
				}
				if found == invert {
					return false
				}
				i, j = end+1, j+1
			default:
				if j >= len(t) || p[i] != t[j] {
					return false
				}
				i, j = i+1, j+1
			}
		}
		return j == len(t)
	}
	return match(0, 0)
}

// likeMatch is SQLite's LIKE: % any run, _ any one character, ASCII letters matching either case,
// esc making the next character literal.
func likeMatch(pattern, s string, esc rune) bool {
	p, t := []rune(pattern), []rune(s)
	fold := func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r - 'A' + 'a'
		}
		return r
	}
	var match func(i, j int) bool
	match = func(i, j int) bool {
		for i < len(p) {
			switch {
			case esc != 0 && p[i] == esc:
				if i+1 >= len(p) || j >= len(t) || fold(p[i+1]) != fold(t[j]) {
					return false
				}
				i, j = i+2, j+1
			case p[i] == '%':
				for i < len(p) && (p[i] == '%' || p[i] == '_') {
					if p[i] == '_' {
						if j >= len(t) {
							return false
						}
						j++
					}
					i++
				}
				if i == len(p) {
					return true
				}
				for k := j; k <= len(t); k++ {
					if match(i, k) {
						return true
					}
				}
				return false
			case p[i] == '_':
				if j >= len(t) {
					return false
				}
				i, j = i+1, j+1
			default:
				if j >= len(t) || fold(p[i]) != fold(t[j]) {
					return false
				}
				i, j = i+1, j+1
			}
		}
		return j == len(t)
	}
	return match(0, 0)
}

// encodeCodePoint is a code point in UTF-8 as SQLite writes it: any value up to U+10FFFF, a surrogate
// included, which Go's encoder would replace.
func encodeCodePoint(cp int64) []byte {
	switch {
	case cp < 0x80:
		return []byte{byte(cp)}
	case cp < 0x800:
		return []byte{0xC0 | byte(cp>>6), 0x80 | byte(cp&0x3F)}
	case cp < 0x10000:
		return []byte{0xE0 | byte(cp>>12), 0x80 | byte(cp>>6&0x3F), 0x80 | byte(cp&0x3F)}
	}
	return []byte{0xF0 | byte(cp>>18), 0x80 | byte(cp>>12&0x3F), 0x80 | byte(cp>>6&0x3F), 0x80 | byte(cp&0x3F)}
}
