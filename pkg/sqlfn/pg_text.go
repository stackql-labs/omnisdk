package sqlfn

import (
	"crypto/md5"
	"encoding/hex"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The Postgres text functions (varlena.c, oracle_compat.c, quote.c), as stackql's Postgres database
// runs them: UTF8, with the C collation and character type, so case is ASCII's alone.

// pgMaxAlloc is MaxAllocSize, the largest allocation Postgres makes.
const pgMaxAlloc = 0x3fffffff

func pgAlloc(n int64) error {
	if n < 0 || n > pgMaxAlloc {
		return pgErrorf("invalid memory alloc request size %d", n)
	}
	return nil
}

// pgOut is a value's type output function, as concat and format print it: unlike its ::text, a
// boolean prints as t or f.
func pgOut(v any) string {
	if b, ok := v.(bool); ok {
		if b {
			return "t"
		}
		return "f"
	}
	s, _ := pgTextOf(v)
	return s
}

func asciiMap(s string, f func(byte) byte) string {
	b := []byte(s)
	for i, c := range b {
		b[i] = f(c)
	}
	return string(b)
}

func asciiLower(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + 'a' - 'A'
	}
	return c
}

func asciiUpper(c byte) byte {
	if c >= 'a' && c <= 'z' {
		return c - ('a' - 'A')
	}
	return c
}

func isASCIIAlnum(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9'
}

// pgSubstring is text_substring: characters start .. start+length-1, counted from 1; a missing
// length is to the end.
func pgSubstring(s string, start int32, length int32, toEnd bool) (string, error) {
	s1 := max(start, 1)
	l1 := int64(-1)
	if !toEnd {
		if length < 0 {
			return "", pgErrorf("negative substring length not allowed")
		}
		e := int64(start) + int64(length)
		if e <= math.MaxInt32 {
			if e < 1 {
				return "", nil
			}
			l1 = e - int64(s1)
		}
	}
	runes := utf8.RuneCountInString(s)
	if s == "" || int(s1) > runes {
		return "", nil
	}
	e1 := int64(runes) + 1
	if l1 > -1 {
		e1 = min(int64(s1)+l1, e1)
	}
	return runeSlice(s, int64(s1)-1, e1-1), nil
}

// runeSlice is s's characters from index i to j, 0-based and half-open.
func runeSlice(s string, i, j int64) string {
	from, to := len(s), len(s)
	n := int64(0)
	for b := range s {
		if n == i {
			from = b
		}
		if n == j {
			to = b
			break
		}
		n++
	}
	if from > to {
		return ""
	}
	return s[from:to]
}

// runeClip is the byte length of s's first n characters (pg_mbcharcliplen).
func runeClip(s string, n int) int {
	if n <= 0 {
		return 0
	}
	c := 0
	for b := range s {
		if c == n {
			return b
		}
		c++
	}
	return len(s)
}

// pgPosition is text_position: the 1-based character position of the first match, 0 for none.
func pgPosition(s, sub string) int32 {
	if sub == "" {
		return 1
	}
	i := strings.Index(s, sub)
	if i < 0 {
		return 0
	}
	return int32(utf8.RuneCountInString(s[:i]) + 1)
}

// pgTrim is dotrim over characters.
func pgTrim(s, set string, left, right bool) string {
	if s == "" || set == "" {
		return s
	}
	in := func(r rune) bool { return strings.ContainsRune(set, r) }
	if left {
		s = strings.TrimLeftFunc(s, in)
	}
	if right {
		s = strings.TrimRightFunc(s, in)
	}
	return s
}

// pgPad is lpad and rpad.
func pgPad(s string, n int32, fill string, left bool) (string, error) {
	if n < 0 {
		n = 0
	}
	s = s[:runeClip(s, int(n))]
	sl := int32(utf8.RuneCountInString(s))
	if fill == "" {
		n = sl
	}
	if err := pgAlloc(4 + 4*int64(n)); err != nil {
		return "", err
	}
	fr := []rune(fill)
	var b strings.Builder
	if !left {
		b.WriteString(s)
	}
	for i := int32(0); i < n-sl; i++ {
		b.WriteRune(fr[int(i)%len(fr)])
	}
	if left {
		b.WriteString(s)
	}
	return b.String(), nil
}

// pgSplitPart is split_part, a negative field counted from the end.
func pgSplitPart(s, sep string, n int32) (string, error) {
	if n == 0 {
		return "", pgErrorf("field position must not be zero")
	}
	if s == "" {
		return "", nil
	}
	var fields []string
	if sep == "" {
		fields = []string{s}
	} else {
		fields = strings.Split(s, sep)
	}
	if n < 0 {
		n += int32(len(fields)) + 1
		if n <= 0 {
			return "", nil
		}
	}
	if int(n) > len(fields) {
		return "", nil
	}
	return fields[n-1], nil
}

// pgSplitText is split_text, string_to_array's and string_to_table's split: a NULL separator
// splits into characters, an empty one leaves the string whole, and a field equal to nullStr is NULL.
func pgSplitText(a []any) (fields []*string, ok bool) {
	if a[0] == nil {
		return nil, false
	}
	s := a[0].(string)
	var nullStr *string
	if len(a) > 2 && a[2] != nil {
		n := a[2].(string)
		nullStr = &n
	}
	add := func(f string) {
		if nullStr != nil && f == *nullStr {
			fields = append(fields, nil)
			return
		}
		fields = append(fields, &f)
	}
	fields = []*string{}
	if a[1] == nil {
		for _, r := range s {
			add(string(r))
		}
		return fields, true
	}
	if s == "" {
		return fields, true
	}
	sep := a[1].(string)
	if sep == "" {
		add(s)
		return fields, true
	}
	for _, f := range strings.Split(s, sep) {
		add(f)
	}
	return fields, true
}

// pgArrayElems is an array's elements as their output text.
func pgArrayElems(v any) []*string {
	switch x := v.(type) {
	case pgTextArr:
		return x
	case pgInt4Arr:
		out := make([]*string, len(x))
		for i, e := range x {
			if e != nil {
				s := strconv.FormatInt(int64(*e), 10)
				out[i] = &s
			}
		}
		return out
	}
	return nil
}

// pgArrayToText is array_to_text_internal.
func pgArrayToText(elems []*string, sep string, nullStr *string) string {
	var b strings.Builder
	printed := false
	for _, e := range elems {
		var v string
		switch {
		case e != nil:
			v = *e
		case nullStr != nil:
			v = *nullStr
		default:
			continue
		}
		if printed {
			b.WriteString(sep)
		}
		b.WriteString(v)
		printed = true
	}
	return b.String()
}

// pgQuoteLiteral is quote_literal_internal: quotes and backslashes doubled, and E” when there is
// a backslash.
func pgQuoteLiteral(s string) string {
	var b strings.Builder
	if strings.IndexByte(s, '\\') >= 0 {
		b.WriteByte('E')
	}
	b.WriteByte('\'')
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' || s[i] == '\\' {
			b.WriteByte(s[i])
		}
		b.WriteByte(s[i])
	}
	b.WriteByte('\'')
	return b.String()
}

// pgQuoteIdent is quote_identifier.
func pgQuoteIdent(s string) string {
	safe := s != "" && (s[0] >= 'a' && s[0] <= 'z' || s[0] == '_')
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_') {
			safe = false
		}
	}
	if safe && pgQuotedKeywords[s] {
		safe = false
	}
	if safe {
		return s
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// pgChr is chr in UTF8.
func pgChr(n int32) (string, error) {
	c := uint32(n)
	if c > 127 {
		if c > 0x10ffff {
			return "", pgErrorf("requested character too large for encoding: %d", n)
		}
		if c >= 0xd800 && c <= 0xdfff {
			return "", pgErrorf("requested character not valid for encoding: %d", n)
		}
		return string(rune(c)), nil
	}
	if c == 0 {
		return "", pgErrorf("null character not permitted")
	}
	return string(rune(c)), nil
}

func pgTranslate(s, from, to string) string {
	if s == "" {
		return s
	}
	fr, tr := []rune(from), []rune(to)
	var b strings.Builder
	for _, r := range s {
		i := -1
		for j, f := range fr {
			if f == r {
				i = j
				break
			}
		}
		switch {
		case i < 0:
			b.WriteRune(r)
		case i < len(tr):
			b.WriteRune(tr[i])
		}
	}
	return b.String()
}

// pgFormat is text_format, over the arguments after the format string.
func pgFormat(f string, args []any) (string, error) {
	unterminated := pgErrorf("unterminated format() type specifier")
	var b strings.Builder
	arg := 1
	nargs := len(args) + 1
	i := 0
	advance := func() error {
		i++
		if i >= len(f) {
			return unterminated
		}
		return nil
	}
	digits := func() (int, bool, error) {
		found, v := false, int64(0)
		for i < len(f) && f[i] >= '0' && f[i] <= '9' {
			v = v*10 + int64(f[i]-'0')
			if v > math.MaxInt32 {
				return 0, false, pgErrorf("number is out of range")
			}
			if err := advance(); err != nil {
				return 0, false, err
			}
			found = true
		}
		return int(v), found, nil
	}
	fetch := func() (any, error) {
		if arg >= nargs {
			return nil, pgErrorf("too few arguments for format()")
		}
		v := args[arg-1]
		arg++
		return v, nil
	}
	for ; i < len(f); i++ {
		if f[i] != '%' {
			b.WriteByte(f[i])
			continue
		}
		if err := advance(); err != nil {
			return "", err
		}
		if f[i] == '%' {
			b.WriteByte('%')
			continue
		}
		argpos, widthpos, minus, width := -1, -1, false, 0
		n, found, err := digits()
		if err != nil {
			return "", err
		}
		parsed := false
		if found {
			if f[i] != '$' {
				width, parsed = n, true
			} else {
				if n == 0 {
					return "", pgErrorf("format specifies argument 0, but arguments are numbered from 1")
				}
				argpos = n
				if err := advance(); err != nil {
					return "", err
				}
			}
		}
		if !parsed {
			for f[i] == '-' {
				minus = true
				if err := advance(); err != nil {
					return "", err
				}
			}
			if f[i] == '*' {
				if err := advance(); err != nil {
					return "", err
				}
				n, found, err := digits()
				if err != nil {
					return "", err
				}
				if found {
					if f[i] != '$' {
						return "", pgErrorf("width argument position must be ended by \"$\"")
					}
					if n == 0 {
						return "", pgErrorf("format specifies argument 0, but arguments are numbered from 1")
					}
					widthpos = n
					if err := advance(); err != nil {
						return "", err
					}
				} else {
					widthpos = 0
				}
			} else if n, found, err := digits(); err != nil {
				return "", err
			} else if found {
				width = n
			}
		}
		conv := f[i]
		if conv != 's' && conv != 'I' && conv != 'L' {
			_, size := utf8.DecodeRuneInString(f[i:])
			return "", pgErrorf("unrecognized format() type specifier \"%s\"", f[i:i+size])
		}
		if widthpos >= 0 {
			if widthpos > 0 {
				arg = widthpos
			}
			w, err := fetch()
			if err != nil {
				return "", err
			}
			switch x := w.(type) {
			case nil:
				width = 0
			case pgInt4:
				width = int(x)
			default:
				n, err := pgInt4In(pgOut(x), "int4")
				if err != nil {
					return "", err
				}
				width = int(n.(pgInt4))
			}
		}
		if argpos > 0 {
			arg = argpos
		}
		v, err := fetch()
		if err != nil {
			return "", err
		}
		var str string
		switch {
		case v == nil && conv == 's':
		case v == nil && conv == 'L':
			str = "NULL"
		case v == nil:
			return "", pgErrorf("null values cannot be formatted as an SQL identifier")
		case conv == 'I':
			str = pgQuoteIdent(pgOut(v))
		case conv == 'L':
			str = pgQuoteLiteral(pgOut(v))
		default:
			str = pgOut(v)
		}
		if width == 0 {
			b.WriteString(str)
			continue
		}
		left := minus
		if width < 0 {
			if width <= math.MinInt32 {
				return "", pgErrorf("number is out of range")
			}
			left, width = true, -width
		}
		pad := strings.Repeat(" ", max(0, width-utf8.RuneCountInString(str)))
		if left {
			b.WriteString(str + pad)
		} else {
			b.WriteString(pad + str)
		}
	}
	return b.String(), nil
}

func init() {
	text1 := func(sig string, f func(string) any) {
		registerPg(sig, func(a []any) (any, error) { return f(a[0].(string)), nil })
	}
	text1("lower(text)", func(s string) any { return asciiMap(s, asciiLower) })
	text1("upper(text)", func(s string) any { return asciiMap(s, asciiUpper) })
	text1("initcap(text)", func(s string) any {
		b := []byte(s)
		was := false
		for i, c := range b {
			if was {
				c = asciiLower(c)
			} else {
				c = asciiUpper(c)
			}
			b[i] = c
			was = isASCIIAlnum(c)
		}
		return string(b)
	})
	for _, n := range []string{"length", "char_length", "character_length"} {
		text1(n+"(text)", func(s string) any { return pgInt4(utf8.RuneCountInString(s)) })
	}
	text1("octet_length(text)", func(s string) any { return pgInt4(len(s)) })
	text1("bit_length(text)", func(s string) any { return pgInt4(8 * len(s)) })
	text1("reverse(text)", func(s string) any {
		r := []rune(s)
		for i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {
			r[i], r[j] = r[j], r[i]
		}
		return string(r)
	})
	text1("ascii(text)", func(s string) any {
		if s == "" {
			return pgInt4(0)
		}
		r, _ := utf8.DecodeRuneInString(s)
		return pgInt4(r)
	})
	text1("md5(text)", func(s string) any {
		h := md5.Sum([]byte(s))
		return hex.EncodeToString(h[:])
	})
	text1("quote_ident(text)", func(s string) any { return pgQuoteIdent(s) })
	text1("quote_literal(text)", func(s string) any { return pgQuoteLiteral(s) })
	text1("btrim(text)", func(s string) any { return pgTrim(s, " ", true, true) })
	text1("ltrim(text)", func(s string) any { return pgTrim(s, " ", true, false) })
	text1("rtrim(text)", func(s string) any { return pgTrim(s, " ", false, true) })

	// quote_literal(anyelement) and quote_nullable(anyelement) are SQL functions over $1::text.
	registerPg("quote_literal(anyelement)", func(a []any) (any, error) {
		s, _ := pgTextOf(a[0])
		return pgQuoteLiteral(s), nil
	})
	quoteNullable := func(a []any) (any, error) {
		if a[0] == nil {
			return "NULL", nil
		}
		s, _ := pgTextOf(a[0])
		return pgQuoteLiteral(s), nil
	}
	registerPg("quote_nullable(text)", quoteNullable)
	registerPg("quote_nullable(anyelement)", quoteNullable)

	for _, n := range []string{"substr", "substring"} {
		registerPg(n+"(text,int4)", func(a []any) (any, error) { return pgSubstring(a[0].(string), i4(a[1]), 0, true) })
		registerPg(n+"(text,int4,int4)", func(a []any) (any, error) {
			return pgSubstring(a[0].(string), i4(a[1]), i4(a[2]), false)
		})
	}
	for _, n := range []string{"strpos", "position"} {
		registerPg(n+"(text,text)", func(a []any) (any, error) { return pgInt4(pgPosition(a[0].(string), a[1].(string))), nil })
	}
	registerPg("replace(text,text,text)", func(a []any) (any, error) {
		s, from := a[0].(string), a[1].(string)
		if s == "" || from == "" {
			return s, nil
		}
		return strings.ReplaceAll(s, from, a[2].(string)), nil
	})
	registerPg("btrim(text,text)", func(a []any) (any, error) { return pgTrim(a[0].(string), a[1].(string), true, true), nil })
	registerPg("ltrim(text,text)", func(a []any) (any, error) { return pgTrim(a[0].(string), a[1].(string), true, false), nil })
	registerPg("rtrim(text,text)", func(a []any) (any, error) { return pgTrim(a[0].(string), a[1].(string), false, true), nil })
	registerPg("lpad(text,int4)", func(a []any) (any, error) { return pgPad(a[0].(string), i4(a[1]), " ", true) })
	registerPg("lpad(text,int4,text)", func(a []any) (any, error) { return pgPad(a[0].(string), i4(a[1]), a[2].(string), true) })
	registerPg("rpad(text,int4)", func(a []any) (any, error) { return pgPad(a[0].(string), i4(a[1]), " ", false) })
	registerPg("rpad(text,int4,text)", func(a []any) (any, error) { return pgPad(a[0].(string), i4(a[1]), a[2].(string), false) })
	registerPg("left(text,int4)", func(a []any) (any, error) {
		s, n := a[0].(string), i4(a[1])
		if n < 0 {
			return s[:runeClip(s, utf8.RuneCountInString(s)+int(n))], nil
		}
		return pgSubstring(s, 1, n, false)
	})
	registerPg("right(text,int4)", func(a []any) (any, error) {
		s, n := a[0].(string), i4(a[1])
		if n < 0 {
			n = -n // as C's int32 negation, INT_MIN staying negative
		} else {
			n = int32(utf8.RuneCountInString(s)) - n
		}
		return s[runeClip(s, int(n)):], nil
	})
	registerPg("repeat(text,int4)", func(a []any) (any, error) {
		s, n := a[0].(string), int64(max(i4(a[1]), 0))
		t := n*int64(len(s)) + 4
		if t > math.MaxInt32 {
			return nil, pgErrorf("requested length too large")
		}
		if err := pgAlloc(t); err != nil {
			return nil, err
		}
		return strings.Repeat(s, int(n)), nil
	})
	registerPg("split_part(text,text,int4)", func(a []any) (any, error) {
		return pgSplitPart(a[0].(string), a[1].(string), i4(a[2]))
	})
	registerPg("starts_with(text,text)", func(a []any) (any, error) {
		return strings.HasPrefix(a[0].(string), a[1].(string)), nil
	})
	registerPg("translate(text,text,text)", func(a []any) (any, error) {
		return pgTranslate(a[0].(string), a[1].(string), a[2].(string)), nil
	})
	registerPg("chr(int4)", func(a []any) (any, error) { return pgChr(i4(a[0])) })
	registerPg("to_hex(int4)", func(a []any) (any, error) { return strconv.FormatUint(uint64(uint32(i4(a[0]))), 16), nil })
	registerPg("to_hex(int8)", func(a []any) (any, error) { return strconv.FormatUint(uint64(a[0].(int64)), 16), nil })

	registerPg("concat(any)", func(a []any) (any, error) {
		var b strings.Builder
		for _, v := range a {
			if v != nil {
				b.WriteString(pgOut(v))
			}
		}
		return b.String(), nil
	})
	registerPg("concat_ws(text,any)", func(a []any) (any, error) {
		if a[0] == nil {
			return nil, nil
		}
		var parts []string
		for _, v := range a[1:] {
			if v != nil {
				parts = append(parts, pgOut(v))
			}
		}
		return strings.Join(parts, a[0].(string)), nil
	})
	format := func(a []any) (any, error) {
		if a[0] == nil {
			return nil, nil
		}
		return pgFormat(a[0].(string), a[1:])
	}
	registerPg("format(text)", format)
	registerPg("format(text,any)", format)

	registerPg("array_to_string(anyarray,text)", func(a []any) (any, error) {
		return pgArrayToText(pgArrayElems(a[0]), a[1].(string), nil), nil
	})
	registerPg("array_to_string(anyarray,text,text)", func(a []any) (any, error) {
		if a[0] == nil || a[1] == nil {
			return nil, nil
		}
		var nullStr *string
		if a[2] != nil {
			s := a[2].(string)
			nullStr = &s
		}
		return pgArrayToText(pgArrayElems(a[0]), a[1].(string), nullStr), nil
	})
	toArray := func(a []any) (any, error) {
		f, ok := pgSplitText(a)
		if !ok {
			return nil, nil
		}
		return pgTextArr(f), nil
	}
	registerPg("string_to_array(text,text)", toArray)
	registerPg("string_to_array(text,text,text)", toArray)
	toTable := func(a []any) (any, error) {
		f, ok := pgSplitText(a)
		if !ok {
			return pgRows{}, nil
		}
		rows := make(pgRows, len(f))
		for i, e := range f {
			if e == nil {
				rows[i] = []any{nil}
			} else {
				rows[i] = []any{*e}
			}
		}
		return rows, nil
	}
	registerPg("string_to_table(text,text)", toTable)
	registerPg("string_to_table(text,text,text)", toTable)
}
