package sqlfn

import (
	"strings"
	"unicode/utf8"
)

// Postgres's JSON lexer and recursive-descent parser, ported from src/common/jsonapi.c. The json
// functions are built, as in Postgres, from semantic callbacks the parser makes as it goes; byte
// offsets into the input stand for its pointers.

type pgJSONToken int

const (
	jtInvalid pgJSONToken = iota
	jtString
	jtNumber
	jtObjectStart
	jtObjectEnd
	jtArrayStart
	jtArrayEnd
	jtComma
	jtColon
	jtTrue
	jtFalse
	jtNull
	jtEnd
)

type pgJSONErr int

const (
	jeSuccess pgJSONErr = iota
	jeEscapingInvalid
	jeEscapingRequired
	jeExpectedArrayFirst
	jeExpectedArrayNext
	jeExpectedColon
	jeExpectedEnd
	jeExpectedJSON
	jeExpectedMore
	jeExpectedObjectFirst
	jeExpectedObjectNext
	jeExpectedString
	jeInvalidToken
	jeUnicodeCodePointZero
	jeUnicodeEscapeFormat
	jeUnicodeHighSurrogate
	jeUnicodeLowSurrogate
)

type pgJSONLex struct {
	input           string
	tokenStart      int // -1 at the end
	tokenTerminator int
	prevTerminator  int
	tokenType       pgJSONToken
	level           int
	strval          *strings.Builder // de-escaped string value, when wanted
}

func newPgJSONLex(s string, needEscapes bool) *pgJSONLex {
	l := &pgJSONLex{input: s}
	if needEscapes {
		l.strval = &strings.Builder{}
	}
	return l
}

// pgJSONSem is the parser's semantic callbacks; nil ones are skipped.
type pgJSONSem struct {
	objectStart, objectEnd, arrayStart, arrayEnd func()
	objectFieldStart, objectFieldEnd             func(fname string, isnull bool)
	arrayElementStart, arrayElementEnd           func(isnull bool)
	scalar                                       func(token string, t pgJSONToken)
}

func jsonAlnum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c >= 0x80
}

func (l *pgJSONLex) lex() pgJSONErr {
	s := l.tokenTerminator
	n := len(l.input)
	for s < n && (l.input[s] == ' ' || l.input[s] == '\t' || l.input[s] == '\n' || l.input[s] == '\r') {
		s++
	}
	l.tokenStart = s
	if s >= n {
		l.tokenStart = -1
		l.prevTerminator = l.tokenTerminator
		l.tokenTerminator = s
		l.tokenType = jtEnd
		return jeSuccess
	}
	punct := func(t pgJSONToken) pgJSONErr {
		l.prevTerminator = l.tokenTerminator
		l.tokenTerminator = s + 1
		l.tokenType = t
		return jeSuccess
	}
	switch c := l.input[s]; c {
	case '{':
		return punct(jtObjectStart)
	case '}':
		return punct(jtObjectEnd)
	case '[':
		return punct(jtArrayStart)
	case ']':
		return punct(jtArrayEnd)
	case ',':
		return punct(jtComma)
	case ':':
		return punct(jtColon)
	case '"':
		if e := l.lexString(); e != jeSuccess {
			return e
		}
		l.tokenType = jtString
		return jeSuccess
	case '-':
		if e := l.lexNumber(s + 1); e != jeSuccess {
			return e
		}
		l.tokenType = jtNumber
		return jeSuccess
	case '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		if e := l.lexNumber(s); e != jeSuccess {
			return e
		}
		l.tokenType = jtNumber
		return jeSuccess
	}
	p := s
	for p < n && jsonAlnum(l.input[p]) {
		p++
	}
	if p == s {
		l.prevTerminator = l.tokenTerminator
		l.tokenTerminator = s + 1
		return jeInvalidToken
	}
	l.prevTerminator = l.tokenTerminator
	l.tokenTerminator = p
	switch l.input[s:p] {
	case "true":
		l.tokenType = jtTrue
	case "null":
		l.tokenType = jtNull
	case "false":
		l.tokenType = jtFalse
	default:
		return jeInvalidToken
	}
	return jeSuccess
}

func (l *pgJSONLex) lexString() pgJSONErr {
	if l.strval != nil {
		l.strval.Reset()
	}
	n := len(l.input)
	s := l.tokenStart
	hi := -1
	for {
		s++
		if s >= n {
			l.tokenTerminator = s
			return jeInvalidToken
		}
		c := l.input[s]
		if c == '"' {
			break
		}
		if c < 32 {
			l.tokenTerminator = s
			return jeEscapingRequired
		}
		if c != '\\' {
			if l.strval != nil {
				if hi != -1 {
					return jeUnicodeLowSurrogate
				}
				l.strval.WriteByte(c)
			}
			continue
		}
		s++
		if s >= n {
			l.tokenTerminator = s
			return jeInvalidToken
		}
		c = l.input[s]
		if c == 'u' {
			ch := 0
			for i := 1; i <= 4; i++ {
				s++
				if s >= n {
					l.tokenTerminator = s
					return jeInvalidToken
				}
				d := l.input[s]
				switch {
				case d >= '0' && d <= '9':
					ch = ch*16 + int(d-'0')
				case d >= 'a' && d <= 'f':
					ch = ch*16 + int(d-'a') + 10
				case d >= 'A' && d <= 'F':
					ch = ch*16 + int(d-'A') + 10
				default:
					_, w := utf8.DecodeRuneInString(l.input[s:])
					l.tokenTerminator = s + w
					return jeUnicodeEscapeFormat
				}
			}
			if l.strval != nil {
				if ch >= 0xD800 && ch <= 0xDBFF {
					if hi != -1 {
						return jeUnicodeHighSurrogate
					}
					hi = ch
					continue
				} else if ch >= 0xDC00 && ch <= 0xDFFF {
					if hi == -1 {
						return jeUnicodeLowSurrogate
					}
					ch = ((hi & 0x3FF) << 10) + (ch & 0x3FF) + 0x10000
					hi = -1
				}
				if hi != -1 {
					return jeUnicodeLowSurrogate
				}
				if ch == 0 {
					return jeUnicodeCodePointZero
				}
				l.strval.WriteRune(rune(ch))
			}
			continue
		}
		if l.strval != nil {
			if hi != -1 {
				return jeUnicodeLowSurrogate
			}
			switch c {
			case '"', '\\', '/':
				l.strval.WriteByte(c)
			case 'b':
				l.strval.WriteByte('\b')
			case 'f':
				l.strval.WriteByte('\f')
			case 'n':
				l.strval.WriteByte('\n')
			case 'r':
				l.strval.WriteByte('\r')
			case 't':
				l.strval.WriteByte('\t')
			default:
				l.tokenStart = s
				_, w := utf8.DecodeRuneInString(l.input[s:])
				l.tokenTerminator = s + w
				return jeEscapingInvalid
			}
		} else if strings.IndexByte("\"\\/bfnrt", c) < 0 {
			l.tokenStart = s
			_, w := utf8.DecodeRuneInString(l.input[s:])
			l.tokenTerminator = s + w
			return jeEscapingInvalid
		}
	}
	if hi != -1 {
		return jeUnicodeLowSurrogate
	}
	l.prevTerminator = l.tokenTerminator
	l.tokenTerminator = s + 1
	return jeSuccess
}

func (l *pgJSONLex) lexNumber(s int) pgJSONErr {
	n := len(l.input)
	isDigit := func(i int) bool { return i < n && l.input[i] >= '0' && l.input[i] <= '9' }
	bad := false
	switch {
	case s < n && l.input[s] == '0':
		s++
	case s < n && l.input[s] >= '1' && l.input[s] <= '9':
		for s++; isDigit(s); s++ {
		}
	default:
		bad = true
	}
	if s < n && l.input[s] == '.' {
		s++
		if !isDigit(s) {
			bad = true
		} else {
			for s++; isDigit(s); s++ {
			}
		}
	}
	if s < n && (l.input[s] == 'e' || l.input[s] == 'E') {
		s++
		if s < n && (l.input[s] == '+' || l.input[s] == '-') {
			s++
		}
		if !isDigit(s) {
			bad = true
		} else {
			for s++; isDigit(s); s++ {
			}
		}
	}
	for ; s < n && jsonAlnum(l.input[s]); s++ {
		bad = true
	}
	l.prevTerminator = l.tokenTerminator
	l.tokenTerminator = s
	if bad {
		return jeInvalidToken
	}
	return jeSuccess
}

type pgJSONCtx int

const (
	jcValue pgJSONCtx = iota
	jcString
	jcArrayStart
	jcArrayNext
	jcObjectStart
	jcObjectLabel
	jcObjectNext
	jcObjectComma
	jcEnd
)

func (l *pgJSONLex) reportParseError(ctx pgJSONCtx) pgJSONErr {
	if l.tokenStart < 0 || l.tokenType == jtEnd {
		return jeExpectedMore
	}
	switch ctx {
	case jcEnd:
		return jeExpectedEnd
	case jcValue:
		return jeExpectedJSON
	case jcString, jcObjectComma:
		return jeExpectedString
	case jcArrayStart:
		return jeExpectedArrayFirst
	case jcArrayNext:
		return jeExpectedArrayNext
	case jcObjectStart:
		return jeExpectedObjectFirst
	case jcObjectLabel:
		return jeExpectedColon
	}
	return jeExpectedObjectNext
}

func (l *pgJSONLex) expect(ctx pgJSONCtx, t pgJSONToken) pgJSONErr {
	if l.tokenType == t {
		return l.lex()
	}
	return l.reportParseError(ctx)
}

// parse is pg_parse_json.
func (l *pgJSONLex) parse(sem *pgJSONSem) pgJSONErr {
	if e := l.lex(); e != jeSuccess {
		return e
	}
	var e pgJSONErr
	switch l.tokenType {
	case jtObjectStart:
		e = l.parseObject(sem)
	case jtArrayStart:
		e = l.parseArray(sem)
	default:
		e = l.parseScalar(sem)
	}
	if e == jeSuccess {
		e = l.expect(jcEnd, jtEnd)
	}
	return e
}

func (l *pgJSONLex) parseScalar(sem *pgJSONSem) pgJSONErr {
	tok := l.tokenType
	if tok != jtString && tok != jtNumber && tok != jtTrue && tok != jtFalse && tok != jtNull {
		return l.reportParseError(jcValue)
	}
	if sem.scalar == nil {
		return l.lex()
	}
	var val string
	if tok == jtString {
		if l.strval != nil {
			val = l.strval.String()
		}
	} else {
		val = l.input[l.tokenStart:l.tokenTerminator]
	}
	if e := l.lex(); e != jeSuccess {
		return e
	}
	sem.scalar(val, tok)
	return jeSuccess
}

func (l *pgJSONLex) parseObjectField(sem *pgJSONSem) pgJSONErr {
	if l.tokenType != jtString {
		return l.reportParseError(jcString)
	}
	fname := ""
	if (sem.objectFieldStart != nil || sem.objectFieldEnd != nil) && l.strval != nil {
		fname = l.strval.String()
	}
	if e := l.lex(); e != jeSuccess {
		return e
	}
	if e := l.expect(jcObjectLabel, jtColon); e != jeSuccess {
		return e
	}
	isnull := l.tokenType == jtNull
	if sem.objectFieldStart != nil {
		sem.objectFieldStart(fname, isnull)
	}
	var e pgJSONErr
	switch l.tokenType {
	case jtObjectStart:
		e = l.parseObject(sem)
	case jtArrayStart:
		e = l.parseArray(sem)
	default:
		e = l.parseScalar(sem)
	}
	if e != jeSuccess {
		return e
	}
	if sem.objectFieldEnd != nil {
		sem.objectFieldEnd(fname, isnull)
	}
	return jeSuccess
}

func (l *pgJSONLex) parseObject(sem *pgJSONSem) pgJSONErr {
	if sem.objectStart != nil {
		sem.objectStart()
	}
	l.level++
	if e := l.lex(); e != jeSuccess {
		return e
	}
	var e pgJSONErr
	switch l.tokenType {
	case jtString:
		e = l.parseObjectField(sem)
		for e == jeSuccess && l.tokenType == jtComma {
			if e = l.lex(); e != jeSuccess {
				break
			}
			e = l.parseObjectField(sem)
		}
	case jtObjectEnd:
	default:
		e = l.reportParseError(jcObjectStart)
	}
	if e != jeSuccess {
		return e
	}
	if e = l.expect(jcObjectNext, jtObjectEnd); e != jeSuccess {
		return e
	}
	l.level--
	if sem.objectEnd != nil {
		sem.objectEnd()
	}
	return jeSuccess
}

func (l *pgJSONLex) parseArrayElement(sem *pgJSONSem) pgJSONErr {
	isnull := l.tokenType == jtNull
	if sem.arrayElementStart != nil {
		sem.arrayElementStart(isnull)
	}
	var e pgJSONErr
	switch l.tokenType {
	case jtObjectStart:
		e = l.parseObject(sem)
	case jtArrayStart:
		e = l.parseArray(sem)
	default:
		e = l.parseScalar(sem)
	}
	if e != jeSuccess {
		return e
	}
	if sem.arrayElementEnd != nil {
		sem.arrayElementEnd(isnull)
	}
	return jeSuccess
}

func (l *pgJSONLex) parseArray(sem *pgJSONSem) pgJSONErr {
	if sem.arrayStart != nil {
		sem.arrayStart()
	}
	l.level++
	e := l.expect(jcArrayStart, jtArrayStart)
	if e == jeSuccess && l.tokenType != jtArrayEnd {
		e = l.parseArrayElement(sem)
		for e == jeSuccess && l.tokenType == jtComma {
			if e = l.lex(); e != jeSuccess {
				break
			}
			e = l.parseArrayElement(sem)
		}
	}
	if e != jeSuccess {
		return e
	}
	if e = l.expect(jcArrayNext, jtArrayEnd); e != jeSuccess {
		return e
	}
	l.level--
	if sem.arrayEnd != nil {
		sem.arrayEnd()
	}
	return jeSuccess
}

// countArrayElements is json_count_array_elements, from an array's start.
func (l *pgJSONLex) countArrayElements() (int, pgJSONErr) {
	c := *l
	c.strval = nil
	c.level++
	count := 0
	if e := c.expect(jcArrayStart, jtArrayStart); e != jeSuccess {
		return 0, e
	}
	if c.tokenType != jtArrayEnd {
		for {
			count++
			if e := c.parseArrayElement(&pgJSONSem{}); e != jeSuccess {
				return 0, e
			}
			if c.tokenType != jtComma {
				break
			}
			if e := c.lex(); e != jeSuccess {
				return 0, e
			}
		}
	}
	if e := c.expect(jcArrayNext, jtArrayEnd); e != jeSuccess {
		return 0, e
	}
	return count, jeSuccess
}

// errorFor is json_ereport_error's message.
func (l *pgJSONLex) errorFor(e pgJSONErr) error {
	tok := ""
	if l.tokenStart >= 0 && l.tokenTerminator >= l.tokenStart {
		tok = l.input[l.tokenStart:l.tokenTerminator]
	}
	detail := map[pgJSONErr]string{
		jeEscapingInvalid:      "Escape sequence \"\\" + tok + "\" is invalid.",
		jeExpectedEnd:          "Expected end of input, but found \"" + tok + "\".",
		jeExpectedArrayFirst:   "Expected array element or \"]\", but found \"" + tok + "\".",
		jeExpectedArrayNext:    "Expected \",\" or \"]\", but found \"" + tok + "\".",
		jeExpectedColon:        "Expected \":\", but found \"" + tok + "\".",
		jeExpectedJSON:         "Expected JSON value, but found \"" + tok + "\".",
		jeExpectedMore:         "The input string ended unexpectedly.",
		jeExpectedObjectFirst:  "Expected string or \"}\", but found \"" + tok + "\".",
		jeExpectedObjectNext:   "Expected \",\" or \"}\", but found \"" + tok + "\".",
		jeExpectedString:       "Expected string, but found \"" + tok + "\".",
		jeInvalidToken:         "Token \"" + tok + "\" is invalid.",
		jeUnicodeCodePointZero: "\\u0000 cannot be converted to text.",
		jeUnicodeEscapeFormat:  "\"\\u\" must be followed by four hexadecimal digits.",
		jeUnicodeHighSurrogate: "Unicode high surrogate must not follow a high surrogate.",
		jeUnicodeLowSurrogate:  "Unicode low surrogate must follow a high surrogate.",
	}[e]
	if e == jeEscapingRequired {
		detail = "Character must be escaped."
	}
	switch e {
	case jeUnicodeCodePointZero, jeUnicodeEscapeFormat, jeUnicodeHighSurrogate, jeUnicodeLowSurrogate:
		return pgErrorf("unsupported Unicode escape sequence: %s", detail)
	}
	return pgErrorf("invalid input syntax for type json: %s", detail)
}

// pgJSONIn is json_in: the text, once it parses.
func pgJSONIn(s string) (any, error) {
	l := newPgJSONLex(s, false)
	if e := l.parse(&pgJSONSem{}); e != jeSuccess {
		return nil, l.errorFor(e)
	}
	return pgJSON(s), nil
}

// pgEscapeJSON is escape_json: s as a JSON string literal.
func pgEscapeJSON(b *strings.Builder, s string) {
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		default:
			if c < ' ' {
				const hex = "0123456789abcdef"
				b.WriteString(`\u00`)
				b.WriteByte(hex[c>>4])
				b.WriteByte(hex[c&0xf])
			} else {
				b.WriteByte(c)
			}
		}
	}
	b.WriteByte('"')
}
