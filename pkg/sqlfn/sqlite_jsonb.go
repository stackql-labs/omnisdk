package sqlfn

import (
	"errors"
	"fmt"
	"math"
	"strings"
)

// The core of SQLite's JSON functions, ported from SQLite 3.53's json.c: JSON text (with SQLite's JSON5
// extensions) is translated into JSONB, SQLite's binary encoding, and every function searches, edits
// and renders JSONB. Byte-for-byte the same JSONB as SQLite's — header sizes included — so the
// jsonb_* functions return the blobs SQLite does, and text comes back exactly as SQLite renders it.
//
// Text is read as SQLite reads it, NUL-terminated: at() is the C view of a string.

// jsonText is TEXT carrying SQLite's JSON subtype. The JSON functions return it, and another JSON
// function given it takes the text as JSON rather than as a string: json_array(json('[1]')) is
// [[1]] where json_array('[1]') is ["[1]"].
type jsonText string

// JSONB element types.
const (
	jsonbNull = iota
	jsonbTrue
	jsonbFalse
	jsonbInt
	jsonbInt5
	jsonbFloat
	jsonbFloat5
	jsonbText
	jsonbTextJ
	jsonbText5
	jsonbTextRaw
	jsonbArray
	jsonbObject
)

var jsonbType = [...]string{
	"null", "true", "false", "integer", "integer",
	"real", "real", "text", "text", "text",
	"text", "array", "object", "", "", "", "",
}

const jsonMaxDepth = 1000

// jsonInvalidChar is what jsonUnescapeOneChar yields for a malformed escape.
const jsonInvalidChar = 0x99999

func jsonIsspace(c byte) bool { return c == 0x09 || c == 0x0a || c == 0x0d || c == 0x20 }

// jsonIsOk is false for the bytes JSON text must escape: control characters, '"', '\\', and '\”
// (special in JSON5).
func jsonIsOk(c byte) bool { return c >= 0x20 && c != '"' && c != '\'' && c != '\\' }

// SQLite's ctype classes.
func sqlIsAlpha(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }
func sqlIsAlnum(c byte) bool { return sqlIsAlpha(c) || isDigitByte(c) }
func sqlIsXdigit(c byte) bool {
	return isDigitByte(c) || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}
func sqlIsIdExtra(c byte) bool {
	return c == '$' || c == '_' || c >= 0x80
}
func jsonId1(c byte) bool { return sqlIsAlpha(c) || sqlIsIdExtra(c) }
func jsonId2(c byte) bool { return sqlIsAlnum(c) || sqlIsIdExtra(c) }

func jsonHexToInt(h byte) uint32 {
	v := int(h)
	v += 9 * (1 & (v >> 6))
	return uint32(v & 0xf)
}

func jsonHexToInt4(z string, i int) uint32 {
	return jsonHexToInt(at(z, i))<<12 + jsonHexToInt(at(z, i+1))<<8 + jsonHexToInt(at(z, i+2))<<4 + jsonHexToInt(at(z, i+3))
}

func jsonIs2Hex(z string, i int) bool { return sqlIsXdigit(at(z, i)) && sqlIsXdigit(at(z, i+1)) }
func jsonIs4Hex(z string, i int) bool { return jsonIs2Hex(z, i) && jsonIs2Hex(z, i+2) }

// strspnSpaces is strspn(z+i, " \t\n\r").
func strspnSpaces(z string, i int) int {
	n := 0
	for jsonIsspace(at(z, i+n)) && at(z, i+n) != 0 {
		n++
	}
	return n
}

// json5Whitespace is the number of bytes of JSON5 whitespace, comments included, at z[i].
func json5Whitespace(z string, i int) int {
	n := 0
	for {
		switch at(z, i+n) {
		case 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x20:
			n++
		case '/':
			if at(z, i+n+1) == '*' && at(z, i+n+2) != 0 {
				j := n + 3
				for at(z, i+j) != '/' || at(z, i+j-1) != '*' {
					if at(z, i+j) == 0 {
						return n
					}
					j++
				}
				n = j + 1
			} else if at(z, i+n+1) == '/' {
				j := n + 2
				for c := at(z, i+j); c != 0; c = at(z, i+j) {
					if c == '\n' || c == '\r' {
						break
					}
					if c == 0xe2 && at(z, i+j+1) == 0x80 && (at(z, i+j+2) == 0xa8 || at(z, i+j+2) == 0xa9) {
						j += 2
						break
					}
					j++
				}
				n = j
				if at(z, i+n) != 0 {
					n++
				}
			} else {
				return n
			}
		case 0xc2:
			if at(z, i+n+1) != 0xa0 {
				return n
			}
			n += 2
		case 0xe1:
			if at(z, i+n+1) != 0x9a || at(z, i+n+2) != 0x80 {
				return n
			}
			n += 3
		case 0xe2:
			if at(z, i+n+1) == 0x80 {
				c := at(z, i+n+2)
				if c < 0x80 {
					return n
				}
				if c <= 0x8a || c == 0xa8 || c == 0xa9 || c == 0xaf {
					n += 3
					continue
				}
			} else if at(z, i+n+1) == 0x81 && at(z, i+n+2) == 0x9f {
				n += 3
				continue
			}
			return n
		case 0xe3:
			if at(z, i+n+1) != 0x80 || at(z, i+n+2) != 0x80 {
				return n
			}
			n += 3
		case 0xef:
			if at(z, i+n+1) != 0xbb || at(z, i+n+2) != 0xbf {
				return n
			}
			n += 3
		default:
			return n
		}
	}
}

// strnicmp is sqlite3StrNICmp(z+i, match, n) == 0: case-insensitive, stopping at a NUL.
func strnicmp(z string, i int, match string, n int) bool {
	for k := 0; k < n; k++ {
		a, b := lowerASCII(at(z, i+k)), lowerASCII(at(match, k))
		if a != b {
			return false
		}
		if a == 0 {
			return true
		}
	}
	return true
}

var nanInfNames = []struct {
	c1, c2 byte
	n      int
	eType  int
	match  string
}{
	{'i', 'I', 3, jsonbFloat, "inf"},
	{'i', 'I', 8, jsonbFloat, "infinity"},
	{'n', 'N', 3, jsonbNull, "NaN"},
	{'q', 'Q', 4, jsonbNull, "QNaN"},
	{'s', 'S', 4, jsonbNull, "SNaN"},
}

// Errors SQLite raises from its JSON functions.
var (
	errMalformedJSON = errors.New("malformed JSON")
	errJSONTooDeep   = errors.New("JSON nested too deep")
	errJSONBlob      = errors.New("JSON cannot hold BLOB values")
)

// jsonString is a JSON text under construction (JsonString).
type jsonString struct {
	b    []byte
	eErr uint8
	err  error // the error a JSTRING_ERR or JSTRING_TOODEEP carries
}

const (
	jstringMalformed = 0x02
	jstringTooDeep   = 0x04
	jstringErr       = 0x08
)

func (p *jsonString) appendRaw(s string) {
	if p.eErr == 0 {
		p.b = append(p.b, s...)
	}
}

func (p *jsonString) appendBytes(s []byte) {
	if p.eErr == 0 {
		p.b = append(p.b, s...)
	}
}

func (p *jsonString) appendChar(c byte) {
	if p.eErr == 0 {
		p.b = append(p.b, c)
	}
}

func (p *jsonString) trimOneChar() {
	if p.eErr == 0 {
		p.b = p.b[:len(p.b)-1]
	}
}

func (p *jsonString) tooDeep() {
	p.eErr |= jstringTooDeep
	p.err = errJSONTooDeep
	p.b = p.b[:0]
}

// appendSeparator appends a comma unless the text so far ends an opening bracket.
func (p *jsonString) appendSeparator() {
	if len(p.b) == 0 {
		return
	}
	if c := p.b[len(p.b)-1]; c == '[' || c == '{' {
		return
	}
	p.appendChar(',')
}

func (p *jsonString) appendControlChar(c byte) {
	special := map[byte]byte{'\b': 'b', '\t': 't', '\n': 'n', '\f': 'f', '\r': 'r'}
	if s, ok := special[c]; ok {
		p.appendChar('\\')
		p.appendChar(s)
		return
	}
	const hex = "0123456789abcdef"
	p.appendRaw("\\u00")
	p.appendChar(hex[c>>4])
	p.appendChar(hex[c&0xf])
}

// appendString appends z quoted as a JSON string, escaping what JSON requires.
func (p *jsonString) appendString(z []byte) {
	p.appendChar('"')
	for _, c := range z {
		switch {
		case jsonIsOk(c) || c == '\'':
			p.appendChar(c)
		case c == '"' || c == '\\':
			p.appendChar('\\')
			p.appendChar(c)
		default:
			p.appendControlChar(c)
		}
	}
	p.appendChar('"')
}

// jsonReal renders a REAL in JSON as SQLite's %!0.17g does: SQLite's REAL text, with an infinity as
// 9.0e+999 so it reads back as one.
func jsonReal(f float64) string { return formatFloat("%!0.17g", f) }

// appendSqlValue appends an SQL value as JSON: NULL as null, numbers as numbers, JSON-subtyped text
// as itself, other text as a string, and a JSONB blob as its JSON.
func (p *jsonString) appendSqlValue(v any) {
	c, x := norm(v)
	switch c {
	case classNull:
		p.appendRaw("null")
	case classReal:
		if math.IsNaN(x.(float64)) {
			p.appendRaw("null") // SQLite holds NaN as NULL
			return
		}
		p.appendRaw(jsonReal(x.(float64)))
	case classInteger:
		p.appendRaw(sqlText(x))
	case classText:
		if _, isJSON := v.(jsonText); isJSON {
			p.appendRaw(x.(string))
		} else {
			p.appendString([]byte(x.(string)))
		}
	default:
		if px, ok := jsonArgIsJsonb(v); ok {
			px.translateBlobToText(0, p)
		} else if p.eErr == 0 {
			p.eErr = jstringErr
			p.err = errJSONBlob
			p.b = p.b[:0]
		}
	}
}

// result is the string as a function result: JSON text, or JSONB when asBlob.
func (p *jsonString) result(asBlob bool) (any, error) {
	switch {
	case p.eErr == 0:
		if asBlob {
			return jsonStringAsBlob(p.b), nil
		}
		return jsonText(p.b), nil
	case p.err != nil:
		return nil, p.err
	}
	return nil, errMalformedJSON
}

// jsonStringAsBlob is well-formed JSON text translated to JSONB.
func jsonStringAsBlob(text []byte) []byte {
	px := &jsonParse{zJson: string(text), owned: true}
	px.translateTextToBlob(0)
	return px.aBlob
}

// jsonParse is JSON as JSONB, with the state of a search or edit of it (JsonParse).
type jsonParse struct {
	aBlob     []byte // the JSONB; its length is nBlob
	owned     bool   // aBlob may be edited in place (nBlobAlloc > 0)
	zJson     string // the text parsed, when it came from text
	hasJson   bool
	iErr      int
	iDepth    int
	nErr      int
	hasNonstd bool
	// Search and edit state; see lookupStep.
	eEdit  int
	delta  int
	aIns   []byte
	iLabel int
}

const (
	jeditDel  = 1
	jeditRepl = 2
	jeditIns  = 3
	jeditSet  = 4
	jeditAins = 5
)

func (p *jsonParse) nBlob() int { return len(p.aBlob) }

// makeEditable copies a borrowed blob so it can be changed.
func (p *jsonParse) makeEditable() {
	if p.owned {
		return
	}
	p.aBlob = append([]byte(nil), p.aBlob...)
	p.owned = true
}

func (p *jsonParse) appendOneByte(c byte) { p.aBlob = append(p.aBlob, c) }

// appendNode appends a node header for eType and a payload of sz bytes, then the payload when given.
func (p *jsonParse) appendNode(eType byte, sz int, payload []byte) {
	switch {
	case sz <= 11:
		p.aBlob = append(p.aBlob, eType|byte(sz<<4))
	case sz <= 0xff:
		p.aBlob = append(p.aBlob, eType|0xc0, byte(sz))
	case sz <= 0xffff:
		p.aBlob = append(p.aBlob, eType|0xd0, byte(sz>>8), byte(sz))
	default:
		p.aBlob = append(p.aBlob, eType|0xe0, byte(sz>>24), byte(sz>>16), byte(sz>>8), byte(sz))
	}
	if payload != nil {
		p.aBlob = append(p.aBlob, payload[:sz]...)
	}
}

// changePayloadSize rewrites the header at i for a payload of sz bytes, growing or shrinking the
// header as needed, and returns the change in the blob's size.
func (p *jsonParse) changePayloadSize(i, sz int) int {
	var nExtra, nNeeded int
	switch szType := p.aBlob[i] >> 4; {
	case szType <= 11:
		nExtra = 0
	case szType == 12:
		nExtra = 1
	case szType == 13:
		nExtra = 2
	case szType == 14:
		nExtra = 4
	default:
		nExtra = 8
	}
	switch {
	case sz <= 11:
		nNeeded = 0
	case sz <= 0xff:
		nNeeded = 1
	case sz <= 0xffff:
		nNeeded = 2
	default:
		nNeeded = 4
	}
	delta := nNeeded - nExtra
	if delta > 0 {
		p.aBlob = append(p.aBlob[:i+1], append(make([]byte, delta), p.aBlob[i+1:]...)...)
	} else if delta < 0 {
		p.aBlob = append(p.aBlob[:i+1], p.aBlob[i+1-delta:]...)
	}
	a := p.aBlob[i:]
	switch nNeeded {
	case 0:
		a[0] = a[0]&0x0f | byte(sz<<4)
	case 1:
		a[0] = a[0]&0x0f | 0xc0
		a[1] = byte(sz)
	case 2:
		a[0] = a[0]&0x0f | 0xd0
		a[1] = byte(sz >> 8)
		a[2] = byte(sz)
	default:
		a[0] = a[0]&0x0f | 0xe0
		a[1] = byte(sz >> 24)
		a[2] = byte(sz >> 16)
		a[3] = byte(sz >> 8)
		a[4] = byte(sz)
	}
	return delta
}

// payloadSize reads the header at i: its size n and the payload size sz; n is 0 for a malformed
// header.
func (p *jsonParse) payloadSize(i int) (n, sz int) { return p.payloadSizeIn(i, p.nBlob()) }

func (p *jsonParse) payloadSizeIn(i, nBlob int) (n, sz int) {
	a := p.aBlob
	if i >= nBlob {
		return 0, 0
	}
	x := a[i] >> 4
	switch {
	case x <= 11:
		sz, n = int(x), 1
	case x == 12:
		if i+1 >= nBlob {
			return 0, 0
		}
		sz, n = int(a[i+1]), 2
	case x == 13:
		if i+2 >= nBlob {
			return 0, 0
		}
		sz, n = int(a[i+1])<<8+int(a[i+2]), 3
	case x == 14:
		if i+4 >= nBlob {
			return 0, 0
		}
		sz, n = int(a[i+1])<<24+int(a[i+2])<<16+int(a[i+3])<<8+int(a[i+4]), 5
	default:
		if i+8 >= nBlob || a[i+1] != 0 || a[i+2] != 0 || a[i+3] != 0 || a[i+4] != 0 {
			return 0, 0
		}
		sz, n = int(a[i+5])<<24+int(a[i+6])<<16+int(a[i+7])<<8+int(a[i+8]), 9
	}
	if i+sz+n > nBlob && i+sz+n > nBlob-p.delta {
		return 0, 0
	}
	return n, sz
}

// jsonIs4HexB reports \uXXXX at z[i] (i at the 'u'), which makes a label TEXTJ.
func jsonIs4HexB(z string, i int, op *byte) bool {
	if at(z, i) != 'u' || !jsonIs4Hex(z, i+1) {
		return false
	}
	*op = jsonbTextJ
	return true
}

// validityCheck checks the element at i, ending at iEnd, and returns 0 or the 1-based offset of the
// first error.
func (p *jsonParse) validityCheck(i, iEnd, iDepth int) int {
	if iDepth > jsonMaxDepth {
		return i + 1
	}
	n, sz := p.payloadSize(i)
	if n == 0 || i+n+sz != iEnd {
		return i + 1
	}
	z := p.aBlob
	zs := string(z)
	x := z[i] & 0x0f
	switch x {
	case jsonbNull, jsonbTrue, jsonbFalse:
		if n+sz == 1 {
			return 0
		}
		return i + 1
	case jsonbInt:
		if sz < 1 {
			return i + 1
		}
		j := i + n
		if z[j] == '-' {
			j++
			if sz < 2 {
				return i + 1
			}
		}
		for k := i + n + sz; j < k; j++ {
			if !isDigitByte(z[j]) {
				return j + 1
			}
		}
		return 0
	case jsonbInt5:
		if sz < 3 {
			return i + 1
		}
		j := i + n
		if z[j] == '-' {
			if sz < 4 {
				return i + 1
			}
			j++
		}
		if z[j] != '0' {
			return i + 1
		}
		if z[j+1] != 'x' && z[j+1] != 'X' {
			return j + 2
		}
		j += 2
		for k := i + n + sz; j < k; j++ {
			if !sqlIsXdigit(z[j]) {
				return j + 1
			}
		}
		return 0
	case jsonbFloat, jsonbFloat5:
		seen := 0
		if sz < 2 {
			return i + 1
		}
		j := i + n
		k := j + sz
		if z[j] == '-' {
			j++
			if sz < 3 {
				return i + 1
			}
		}
		if z[j] == '.' {
			if x == jsonbFloat {
				return j + 1
			}
			if !isDigitByte(at(zs, j+1)) {
				return j + 1
			}
			j += 2
			seen = 1
		} else if z[j] == '0' && x == jsonbFloat {
			if j+3 > k {
				return j + 1
			}
			if c := at(zs, j+1); c != '.' && c != 'e' && c != 'E' {
				return j + 1
			}
			j++
		}
		for ; j < k; j++ {
			if isDigitByte(z[j]) {
				continue
			}
			if z[j] == '.' {
				if seen > 0 {
					return j + 1
				}
				if x == jsonbFloat && (j == k-1 || !isDigitByte(z[j+1])) {
					return j + 1
				}
				seen = 1
				continue
			}
			if z[j] == 'e' || z[j] == 'E' {
				if seen == 2 {
					return j + 1
				}
				if j == k-1 {
					return j + 1
				}
				if z[j+1] == '+' || z[j+1] == '-' {
					j++
					if j == k-1 {
						return j + 1
					}
				}
				seen = 2
				continue
			}
			return j + 1
		}
		if seen == 0 {
			return i + 1
		}
		return 0
	case jsonbText:
		for j, k := i+n, i+n+sz; j < k; j++ {
			if !jsonIsOk(z[j]) && z[j] != '\'' {
				return j + 1
			}
		}
		return 0
	case jsonbTextJ, jsonbText5:
		for j, k := i+n, i+n+sz; j < k; j++ {
			if jsonIsOk(z[j]) || z[j] == '\'' {
				continue
			}
			switch {
			case z[j] == '"':
				if x == jsonbTextJ {
					return j + 1
				}
			case z[j] <= 0x1f:
				if x == jsonbTextJ {
					return j + 1
				}
			case z[j] != '\\' || j+1 >= k:
				return j + 1
			case strings.IndexByte("\"\\/bfnrt", z[j+1]) >= 0:
				j++
			case z[j+1] == 'u':
				if j+5 >= k {
					return j + 1
				}
				if !jsonIs4Hex(zs, j+2) {
					return j + 1
				}
				j++
			case x != jsonbText5:
				return j + 1
			default:
				szC, c := jsonUnescapeOneChar(zs[j:k])
				if c == jsonInvalidChar {
					return j + 1
				}
				j += szC - 1
			}
		}
		return 0
	case jsonbTextRaw:
		return 0
	case jsonbArray:
		j, k := i+n, i+n+sz
		for j < k {
			n, sz := p.payloadSize(j)
			if n == 0 || j+n+sz > k {
				return j + 1
			}
			if sub := p.validityCheck(j, j+n+sz, iDepth+1); sub != 0 {
				return sub
			}
			j += n + sz
		}
		return 0
	case jsonbObject:
		cnt := 0
		j, k := i+n, i+n+sz
		for j < k {
			n, sz := p.payloadSize(j)
			if n == 0 || j+n+sz > k {
				return j + 1
			}
			if cnt&1 == 0 {
				if x := z[j] & 0x0f; x < jsonbText || x > jsonbTextRaw {
					return j + 1
				}
			}
			if sub := p.validityCheck(j, j+n+sz, iDepth+1); sub != 0 {
				return sub
			}
			cnt++
			j += n + sz
		}
		if cnt&1 != 0 {
			return j + 1
		}
		return 0
	}
	return i + 1
}

// translateTextToBlob translates the element of JSON text at zJson[i] to JSONB, appended to aBlob. It
// returns the index past the element, or 0 at the end of input, -1 on a syntax error, and -2..-5 on
// '}' ']' ',' ':' (with iErr at that character).
func (p *jsonParse) translateTextToBlob(i int) int {
	z := p.zJson
restart:
	switch c := at(z, i); c {
	case '{':
		iThis := p.nBlob()
		p.appendNode(jsonbObject, len(z)-i, nil)
		p.iDepth++
		if p.iDepth > jsonMaxDepth {
			p.iErr = i
			return -1
		}
		iStart := p.nBlob()
		j := i + 1
		for ; ; j++ {
			iBlob := p.nBlob()
			x := p.translateTextToBlob(j)
			if x <= 0 {
				if x == -2 {
					j = p.iErr
					if p.nBlob() != iStart {
						p.hasNonstd = true
					}
					break
				}
				j += json5Whitespace(z, j)
				op := byte(jsonbText)
				if jsonId1(at(z, j)) || (at(z, j) == '\\' && jsonIs4HexB(z, j+1, &op)) {
					k := j + 1
					for (jsonId2(at(z, k)) && json5Whitespace(z, k) == 0) || (at(z, k) == '\\' && jsonIs4HexB(z, k+1, &op)) {
						k++
					}
					p.appendNode(op, k-j, []byte(z[j:k]))
					p.hasNonstd = true
					x = k
				} else {
					if x != -1 {
						p.iErr = j
					}
					return -1
				}
			}
			if t := p.aBlob[iBlob] & 0x0f; t < jsonbText || t > jsonbTextRaw {
				p.iErr = j
				return -1
			}
			j = x
			if at(z, j) == ':' {
				j++
			} else {
				gotColon := false
				if jsonIsspace(at(z, j)) {
					for {
						j++
						if !jsonIsspace(at(z, j)) {
							break
						}
					}
					if at(z, j) == ':' {
						j++
						gotColon = true
					}
				}
				if !gotColon {
					x = p.translateTextToBlob(j)
					if x != -5 {
						if x != -1 {
							p.iErr = j
						}
						return -1
					}
					j = p.iErr + 1
				}
			}
			x = p.translateTextToBlob(j)
			if x <= 0 {
				if x != -1 {
					p.iErr = j
				}
				return -1
			}
			j = x
			if at(z, j) == ',' {
				continue
			} else if at(z, j) == '}' {
				break
			}
			if jsonIsspace(at(z, j)) {
				j += 1 + strspnSpaces(z, j+1)
				if at(z, j) == ',' {
					continue
				} else if at(z, j) == '}' {
					break
				}
			}
			x = p.translateTextToBlob(j)
			if x == -4 {
				j = p.iErr
				continue
			}
			if x == -2 {
				j = p.iErr
				break
			}
			p.iErr = j
			return -1
		}
		p.changePayloadSize(iThis, p.nBlob()-iStart)
		p.iDepth--
		return j + 1
	case '[':
		iThis := p.nBlob()
		p.appendNode(jsonbArray, len(z)-i, nil)
		iStart := p.nBlob()
		p.iDepth++
		if p.iDepth > jsonMaxDepth {
			p.iErr = i
			return -1
		}
		j := i + 1
		for ; ; j++ {
			x := p.translateTextToBlob(j)
			if x <= 0 {
				if x == -3 {
					j = p.iErr
					if p.nBlob() != iStart {
						p.hasNonstd = true
					}
					break
				}
				if x != -1 {
					p.iErr = j
				}
				return -1
			}
			j = x
			if at(z, j) == ',' {
				continue
			} else if at(z, j) == ']' {
				break
			}
			if jsonIsspace(at(z, j)) {
				j += 1 + strspnSpaces(z, j+1)
				if at(z, j) == ',' {
					continue
				} else if at(z, j) == ']' {
					break
				}
			}
			x = p.translateTextToBlob(j)
			if x == -4 {
				j = p.iErr
				continue
			}
			if x == -3 {
				j = p.iErr
				break
			}
			p.iErr = j
			return -1
		}
		p.changePayloadSize(iThis, p.nBlob()-iStart)
		p.iDepth--
		return j + 1
	case '\'', '"':
		if c == '\'' {
			p.hasNonstd = true
		}
		opcode := byte(jsonbText)
		cDelim := c
		j := i + 1
		for {
			if jsonIsOk(at(z, j)) {
				if !jsonIsOk(at(z, j+1)) {
					j++
				} else if !jsonIsOk(at(z, j+2)) {
					j += 2
				} else {
					j += 3
					continue
				}
			}
			c := at(z, j)
			if c == cDelim {
				break
			} else if c == '\\' {
				j++
				c = at(z, j)
				switch {
				case c == '"' || c == '\\' || c == '/' || c == 'b' || c == 'f' || c == 'n' || c == 'r' || c == 't' ||
					(c == 'u' && jsonIs4Hex(z, j+1)):
					if opcode == jsonbText {
						opcode = jsonbTextJ
					}
				case c == '\'' || c == 'v' || c == '\n' || (c == '0' && !isDigitByte(at(z, j+1))) ||
					(c == 0xe2 && at(z, j+1) == 0x80 && (at(z, j+2) == 0xa8 || at(z, j+2) == 0xa9)) ||
					(c == 'x' && jsonIs2Hex(z, j+1)):
					opcode = jsonbText5
					p.hasNonstd = true
				case c == '\r':
					if at(z, j+1) == '\n' {
						j++
					}
					opcode = jsonbText5
					p.hasNonstd = true
				default:
					p.iErr = j
					return -1
				}
			} else if c <= 0x1f {
				if c == 0 {
					p.iErr = j
					return -1
				}
				// Control characters are JSON5, not RFC-8259.
				opcode = jsonbText5
				p.hasNonstd = true
			} else if c == '"' {
				opcode = jsonbText5
			}
			j++
		}
		p.appendNode(opcode, j-1-i, []byte(z[i+1:j]))
		return j + 1
	case 't':
		if strings.HasPrefix(z[i:], "true") && !sqlIsAlnum(at(z, i+4)) {
			p.appendOneByte(jsonbTrue)
			return i + 4
		}
		p.iErr = i
		return -1
	case 'f':
		if strings.HasPrefix(z[i:], "false") && !sqlIsAlnum(at(z, i+5)) {
			p.appendOneByte(jsonbFalse)
			return i + 5
		}
		p.iErr = i
		return -1
	case '+', '.', '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return p.translateNumber(i)
	case '}':
		p.iErr = i
		return -2
	case ']':
		p.iErr = i
		return -3
	case ',':
		p.iErr = i
		return -4
	case ':':
		p.iErr = i
		return -5
	case 0:
		return 0
	case 0x09, 0x0a, 0x0d, 0x20:
		i += 1 + strspnSpaces(z, i+1)
		goto restart
	case 0x0b, 0x0c, '/', 0xc2, 0xe1, 0xe2, 0xe3, 0xef:
		if j := json5Whitespace(z, i); j > 0 {
			i += j
			p.hasNonstd = true
			goto restart
		}
		p.iErr = i
		return -1
	case 'n':
		if strings.HasPrefix(z[i:], "null") && !sqlIsAlnum(at(z, i+4)) {
			p.appendOneByte(jsonbNull)
			return i + 4
		}
		return p.translateNanInf(i)
	default:
		return p.translateNanInf(i)
	}
}

func (p *jsonParse) translateNanInf(i int) int {
	z := p.zJson
	c := at(z, i)
	for _, k := range nanInfNames {
		if c != k.c1 && c != k.c2 {
			continue
		}
		if !strnicmp(z, i, k.match, k.n) {
			continue
		}
		if sqlIsAlnum(at(z, i+k.n)) {
			continue
		}
		if k.eType == jsonbFloat {
			p.appendNode(jsonbFloat, 5, []byte("9e999"))
		} else {
			p.appendOneByte(jsonbNull)
		}
		p.hasNonstd = true
		return i + k.n
	}
	p.iErr = i
	return -1
}

// translateNumber is the number cases of translateTextToBlob. t's bit 0x01 marks JSON5, 0x02 a float.
func (p *jsonParse) translateNumber(i int) int {
	z := p.zJson
	var t byte
	var j int
	seenE := false
	c := at(z, i)
	switch c {
	case '+':
		p.hasNonstd = true
	case '.':
		if !isDigitByte(at(z, i+1)) {
			p.iErr = i
			return -1
		}
		p.hasNonstd = true
		t = 0x03
		goto parseNumber2
	}
	if c <= '0' {
		if c == '0' {
			if (at(z, i+1) == 'x' || at(z, i+1) == 'X') && sqlIsXdigit(at(z, i+2)) {
				p.hasNonstd = true
				t = 0x01
				for j = i + 3; sqlIsXdigit(at(z, j)); j++ {
				}
				goto finish
			} else if isDigitByte(at(z, i+1)) {
				p.iErr = i + 1
				return -1
			}
		} else {
			if !isDigitByte(at(z, i+1)) {
				// JSON5's ±Infinity, and SQLite's ±inf in any case.
				if (at(z, i+1) == 'I' || at(z, i+1) == 'i') && strnicmp(z, i+1, "inf", 3) {
					p.hasNonstd = true
					if c == '-' {
						p.appendNode(jsonbFloat, 6, []byte("-9e999"))
					} else {
						p.appendNode(jsonbFloat, 5, []byte("9e999"))
					}
					if strnicmp(z, i+4, "inity", 5) {
						return i + 9
					}
					return i + 4
				}
				if at(z, i+1) == '.' {
					p.hasNonstd = true
					t |= 0x01
					goto parseNumber2
				}
				p.iErr = i
				return -1
			}
			if at(z, i+1) == '0' {
				if isDigitByte(at(z, i+2)) {
					p.iErr = i + 1
					return -1
				} else if (at(z, i+2) == 'x' || at(z, i+2) == 'X') && sqlIsXdigit(at(z, i+3)) {
					p.hasNonstd = true
					t |= 0x01
					for j = i + 4; sqlIsXdigit(at(z, j)); j++ {
					}
					goto finish
				}
			}
		}
	}
parseNumber2:
	for j = i + 1; ; j++ {
		c := at(z, j)
		if isDigitByte(c) {
			continue
		}
		if c == '.' {
			if t&0x02 != 0 {
				p.iErr = j
				return -1
			}
			t |= 0x02
			continue
		}
		if c == 'e' || c == 'E' {
			if at(z, j-1) < '0' {
				if at(z, j-1) == '.' && j-2 >= i && isDigitByte(at(z, j-2)) {
					p.hasNonstd = true
					t |= 0x01
				} else {
					p.iErr = j
					return -1
				}
			}
			if seenE {
				p.iErr = j
				return -1
			}
			t |= 0x02
			seenE = true
			c = at(z, j+1)
			if c == '+' || c == '-' {
				j++
				c = at(z, j+1)
			}
			if c < '0' || c > '9' {
				p.iErr = j
				return -1
			}
			continue
		}
		break
	}
	if at(z, j-1) < '0' {
		if at(z, j-1) == '.' && j-2 >= i && isDigitByte(at(z, j-2)) {
			p.hasNonstd = true
			t |= 0x01
		} else {
			p.iErr = j
			return -1
		}
	}
finish:
	if at(z, i) == '+' {
		i++
	}
	p.appendNode(jsonbInt+t, j-i, []byte(z[i:j]))
	return j
}

// convertTextToBlob parses zJson whole; false on any error.
func (p *jsonParse) convertTextToBlob() bool {
	z := p.zJson
	i := p.translateTextToBlob(0)
	if i > 0 {
		for jsonIsspace(at(z, i)) {
			i++
		}
		if at(z, i) != 0 {
			i += json5Whitespace(z, i)
			if at(z, i) != 0 {
				p.aBlob = nil
				return false
			}
			p.hasNonstd = true
		}
	}
	if i <= 0 {
		p.aBlob = nil
		return false
	}
	return true
}

// translateBlobToText renders the element at i as JSON text into out, returning the index past it.
func (p *jsonParse) translateBlobToText(i int, out *jsonString) int {
	n, sz := p.payloadSize(i)
	if n == 0 {
		out.eErr |= jstringMalformed
		return p.nBlob() + 1
	}
	a := p.aBlob
	switch a[i] & 0x0f {
	case jsonbNull:
		out.appendRaw("null")
		return i + 1
	case jsonbTrue:
		out.appendRaw("true")
		return i + 1
	case jsonbFalse:
		out.appendRaw("false")
		return i + 1
	case jsonbInt, jsonbFloat:
		if sz == 0 {
			out.eErr |= jstringMalformed
			break
		}
		out.appendBytes(a[i+n : i+n+sz])
	case jsonbInt5:
		if sz == 0 {
			out.eErr |= jstringMalformed
			break
		}
		zIn := a[i+n : i+n+sz]
		k := 2
		var u uint64
		overflow := false
		if zIn[0] == '-' {
			out.appendChar('-')
			k++
		} else if zIn[0] == '+' {
			k++
		}
		for ; k < sz; k++ {
			if !sqlIsXdigit(zIn[k]) {
				out.eErr |= jstringMalformed
				break
			} else if u>>60 != 0 {
				overflow = true
			} else {
				u = u*16 + uint64(jsonHexToInt(zIn[k]))
			}
		}
		if overflow {
			out.appendRaw("9.0e999")
		} else {
			out.appendRaw(fmt.Sprintf("%d", u))
		}
	case jsonbFloat5:
		if sz == 0 {
			out.eErr |= jstringMalformed
			break
		}
		zIn := a[i+n : i+n+sz]
		k := 0
		if zIn[0] == '-' {
			out.appendChar('-')
			if sz <= 1 {
				out.eErr |= jstringMalformed
				break
			}
			k = 1
		}
		if zIn[k] == '.' {
			out.appendChar('0')
		}
		for ; k < sz; k++ {
			out.appendChar(zIn[k])
			if zIn[k] == '.' && (k+1 == sz || !isDigitByte(zIn[k+1])) {
				out.appendChar('0')
			}
		}
	case jsonbText, jsonbTextJ:
		out.appendChar('"')
		out.appendBytes(a[i+n : i+n+sz])
		out.appendChar('"')
	case jsonbText5:
		zIn := a[i+n : i+n+sz]
		out.appendChar('"')
		for len(zIn) > 0 {
			k := 0
			for k < len(zIn) && (jsonIsOk(zIn[k]) || zIn[k] == '\'') {
				k++
			}
			if k > 0 {
				out.appendBytes(zIn[:k])
				if k >= len(zIn) {
					break
				}
				zIn = zIn[k:]
			}
			if zIn[0] == '"' {
				out.appendRaw("\\\"")
				zIn = zIn[1:]
				continue
			}
			if zIn[0] <= 0x1f {
				out.appendControlChar(zIn[0])
				zIn = zIn[1:]
				continue
			}
			if len(zIn) < 2 {
				out.eErr |= jstringMalformed
				break
			}
			switch zIn[1] {
			case '\'':
				out.appendChar('\'')
			case 'v':
				out.appendRaw("\\u000b")
			case 'x':
				if len(zIn) < 4 {
					out.eErr |= jstringMalformed
					zIn = zIn[:2]
					break
				}
				out.appendRaw("\\u00")
				out.appendBytes(zIn[2:4])
				zIn = zIn[2:]
			case '0':
				out.appendRaw("\\u0000")
			case '\r':
				if len(zIn) > 2 && zIn[2] == '\n' {
					zIn = zIn[1:]
				}
			case '\n':
			case 0xe2:
				// A backslash before U+2028 or U+2029 is a line continuation.
				if len(zIn) < 4 || zIn[2] != 0x80 || (zIn[3] != 0xa8 && zIn[3] != 0xa9) {
					out.eErr |= jstringMalformed
					zIn = zIn[:2]
					break
				}
				zIn = zIn[2:]
			default:
				out.appendBytes(zIn[:2])
			}
			zIn = zIn[2:]
		}
		out.appendChar('"')
	case jsonbTextRaw:
		out.appendString(a[i+n : i+n+sz])
	case jsonbArray:
		out.appendChar('[')
		j, iEnd := i+n, i+n+sz
		p.iDepth++
		if p.iDepth > jsonMaxDepth {
			out.tooDeep()
		}
		for j < iEnd && out.eErr == 0 {
			j = p.translateBlobToText(j, out)
			out.appendChar(',')
		}
		p.iDepth--
		if j > iEnd {
			out.eErr |= jstringMalformed
		}
		if sz > 0 {
			out.trimOneChar()
		}
		out.appendChar(']')
	case jsonbObject:
		x := 0
		out.appendChar('{')
		j, iEnd := i+n, i+n+sz
		p.iDepth++
		if p.iDepth > jsonMaxDepth {
			out.tooDeep()
		}
		for j < iEnd && out.eErr == 0 {
			j = p.translateBlobToText(j, out)
			if x&1 != 0 {
				out.appendChar(',')
			} else {
				out.appendChar(':')
			}
			x++
		}
		p.iDepth--
		if x&1 != 0 || j > iEnd {
			out.eErr |= jstringMalformed
		}
		if sz > 0 {
			out.trimOneChar()
		}
		out.appendChar('}')
	default:
		out.eErr |= jstringMalformed
	}
	return i + n + sz
}

// jsonPretty is json_pretty's rendering state.
type jsonPretty struct {
	p       *jsonParse
	out     *jsonString
	indent  string
	nIndent int
}

func (x *jsonPretty) appendIndent() {
	for k := 0; k < x.nIndent; k++ {
		x.out.appendRaw(x.indent)
	}
}

func (x *jsonPretty) translate(i int) int {
	p, out := x.p, x.out
	n, sz := p.payloadSize(i)
	if n == 0 {
		out.eErr |= jstringMalformed
		return p.nBlob() + 1
	}
	switch p.aBlob[i] & 0x0f {
	case jsonbArray:
		j, iEnd := i+n, i+n+sz
		out.appendChar('[')
		if j < iEnd {
			out.appendChar('\n')
			x.nIndent++
			if x.nIndent >= jsonMaxDepth {
				out.tooDeep()
			}
			for out.eErr == 0 {
				x.appendIndent()
				j = x.translate(j)
				if j >= iEnd {
					break
				}
				out.appendRaw(",\n")
			}
			out.appendChar('\n')
			x.nIndent--
			x.appendIndent()
		}
		out.appendChar(']')
		return iEnd
	case jsonbObject:
		j, iEnd := i+n, i+n+sz
		out.appendChar('{')
		if j < iEnd {
			out.appendChar('\n')
			x.nIndent++
			if x.nIndent >= jsonMaxDepth {
				out.tooDeep()
			}
			p.iDepth = x.nIndent
			for out.eErr == 0 {
				x.appendIndent()
				j = p.translateBlobToText(j, out)
				if j > iEnd {
					out.eErr |= jstringMalformed
					break
				}
				out.appendRaw(": ")
				j = x.translate(j)
				if j >= iEnd {
					break
				}
				out.appendRaw(",\n")
			}
			out.appendChar('\n')
			x.nIndent--
			x.appendIndent()
		}
		out.appendChar('}')
		return iEnd
	}
	return p.translateBlobToText(i, out)
}

// arrayCount is the number of elements of the array at iRoot.
func (p *jsonParse) arrayCount(iRoot int) int {
	n, sz := p.payloadSize(iRoot)
	iEnd := iRoot + n + sz
	k := 0
	for i := iRoot + n; n > 0 && i < iEnd; k++ {
		n, sz = p.payloadSize(i)
		i += sz + n
	}
	return k
}

// afterEditSizeAdjust corrects the payload size at iRoot by delta.
func (p *jsonParse) afterEditSizeAdjust(iRoot int) {
	_, sz := p.payloadSizeIn(iRoot, math.MaxInt32)
	sz += p.delta
	p.delta += p.changePayloadSize(iRoot, sz)
}

// blobOverwrite writes aIns over aOut with its header widened by d bytes, so an overwrite need not
// move the rest of the blob; false where the header cannot widen by d.
func blobOverwrite(aOut []byte, aIns []byte, d int) bool {
	aType := [...]byte{0xc0, 0xd0, 0, 0xe0, 0, 0, 0, 0xf0}
	if aIns[0]&0x0f <= 2 {
		return false
	}
	var i, szHdr int
	switch aIns[0] >> 4 {
	default:
		if (1<<d)&0x116 == 0 {
			return false
		}
		i, szHdr = d+1, 1
	case 12:
		if (1<<d)&0x8a == 0 {
			return false
		}
		i, szHdr = d+2, 2
	case 13:
		if d != 2 && d != 6 {
			return false
		}
		i, szHdr = d+3, 3
	case 14:
		if d != 4 {
			return false
		}
		i, szHdr = 9, 5
	case 15:
		return false
	}
	aOut[0] = aIns[0]&0x0f | aType[i-2]
	copy(aOut[i:], aIns[szHdr:])
	szPayload := len(aIns) - szHdr
	for {
		i--
		aOut[i] = byte(szPayload)
		if i == 1 {
			break
		}
		szPayload >>= 8
	}
	return true
}

// blobEdit replaces nDel bytes at iDel with aIns, or with nIns zero bytes when aIns is nil.
func (p *jsonParse) blobEdit(iDel, nDel int, aIns []byte, nIns int) {
	if aIns != nil {
		nIns = len(aIns)
	}
	d := nIns - nDel
	if d < 0 && d >= -8 && aIns != nil && blobOverwrite(p.aBlob[iDel:], aIns, -d) {
		return
	}
	if d != 0 {
		rest := append([]byte(nil), p.aBlob[iDel+nDel:]...)
		p.aBlob = append(append(p.aBlob[:iDel], make([]byte, nIns)...), rest...)
		p.delta += d
	}
	if nIns > 0 && aIns != nil {
		copy(p.aBlob[iDel:], aIns)
	}
}

// jsonBytesToBypass is the length of escaped newlines at the start of z.
func jsonBytesToBypass(z string) int {
	i, n := 0, len(z)
	for i+1 < n {
		if z[i] != '\\' {
			return i
		}
		if z[i+1] == '\n' {
			i += 2
			continue
		}
		if z[i+1] == '\r' {
			if i+2 < n && z[i+2] == '\n' {
				i += 3
			} else {
				i += 2
			}
			continue
		}
		if z[i+1] == 0xe2 && i+3 < n && z[i+2] == 0x80 && (z[i+3] == 0xa8 || z[i+3] == 0xa9) {
			i += 4
			continue
		}
		break
	}
	return i
}

var utf8Trans1 = [...]uint32{
	0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f,
	0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18, 0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f,
	0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f,
	0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x00, 0x01, 0x02, 0x03, 0x00, 0x01, 0x00, 0x00,
}

// utf8ReadLimited is sqlite3Utf8ReadLimited: one character from at most four bytes, however
// malformed; it returns the bytes read and the character.
func utf8ReadLimited(z string) (int, uint32) {
	n := len(z)
	c := uint32(z[0])
	i := 1
	if c >= 0xc0 {
		c = utf8Trans1[c-0xc0]
		if n > 4 {
			n = 4
		}
		for i < n && z[i]&0xc0 == 0x80 {
			c = c<<6 + uint32(0x3f&z[i])
			i++
		}
	}
	return i, c
}

// jsonUnescapeOneChar decodes the escape at the start of z (a backslash first): the bytes it spans
// and the character, jsonInvalidChar when malformed.
func jsonUnescapeOneChar(z string) (int, uint32) {
	n := len(z)
	if n < 2 {
		return n, jsonInvalidChar
	}
	switch z[1] {
	case 'u':
		if n < 6 {
			return n, jsonInvalidChar
		}
		v := jsonHexToInt4(z, 2)
		if v&0xfc00 == 0xd800 && n >= 12 && z[6] == '\\' && z[7] == 'u' {
			if vlo := jsonHexToInt4(z, 8); vlo&0xfc00 == 0xdc00 {
				return 12, (v&0x3ff)<<10 + vlo&0x3ff + 0x10000
			}
		}
		return 6, v
	case 'b':
		return 2, '\b'
	case 'f':
		return 2, '\f'
	case 'n':
		return 2, '\n'
	case 'r':
		return 2, '\r'
	case 't':
		return 2, '\t'
	case 'v':
		return 2, '\v'
	case '0':
		if n > 2 && isDigitByte(z[2]) {
			return 2, jsonInvalidChar
		}
		return 2, 0
	case '\'', '"', '/', '\\':
		return 2, uint32(z[1])
	case 'x':
		if n < 4 {
			return n, jsonInvalidChar
		}
		return 4, jsonHexToInt(z[2])<<4 | jsonHexToInt(z[3])
	case 0xe2, '\r', '\n':
		nSkip := jsonBytesToBypass(z)
		switch {
		case nSkip == 0:
			return n, jsonInvalidChar
		case nSkip == n:
			return n, 0
		case z[nSkip] == '\\':
			k, c := jsonUnescapeOneChar(z[nSkip:])
			return nSkip + k, c
		}
		k, c := utf8ReadLimited(z[nSkip:])
		return nSkip + k, c
	}
	return 2, jsonInvalidChar
}

// labelCompare reports whether two object labels are equal, decoding escapes in any not raw.
func labelCompare(left string, rawLeft bool, right string, rawRight bool) bool {
	if rawLeft && rawRight {
		return left == right
	}
	next := func(z *string, raw bool) uint32 {
		s := *z
		if len(s) == 0 {
			return 0
		}
		if raw || s[0] != '\\' {
			c := uint32(s[0])
			if c >= 0xc0 {
				k, r := utf8ReadLimited(s)
				*z = s[k:]
				return r
			}
			*z = s[1:]
			return c
		}
		k, c := jsonUnescapeOneChar(s)
		*z = s[k:]
		return c
	}
	for {
		cl, cr := next(&left, rawLeft), next(&right, rawRight)
		if cl != cr {
			return false
		}
		if cl == 0 {
			return true
		}
	}
}

// Results of lookupStep beyond any blob index.
const (
	lookupError     = 0xffffffff
	lookupNotFound  = 0xfffffffe
	lookupNotArray  = 0xfffffffd
	lookupTooDeep   = 0xfffffffc
	lookupPathError = 0xfffffffb
)

func lookupIsError(x int) bool { return x >= lookupPathError }

// createEditSubstructure builds the JSONB to insert for the path tail zTail: the value itself, or the
// objects and arrays the rest of the path calls for around it.
func (p *jsonParse) createEditSubstructure(path string, pos int) (*jsonParse, int) {
	ins := &jsonParse{}
	if at(path, pos) == 0 {
		ins.aBlob = p.aIns
		return ins, 0
	}
	if at(path, pos) == '.' {
		ins.aBlob = []byte{jsonbObject}
	} else {
		ins.aBlob = []byte{jsonbArray}
	}
	ins.eEdit = p.eEdit
	ins.aIns = p.aIns
	ins.iDepth = p.iDepth + 1
	if ins.iDepth >= jsonMaxDepth {
		return ins, lookupTooDeep
	}
	rc := ins.lookupStep(0, path, pos, 0)
	p.iDepth--
	return ins, rc
}

// lookupStep follows path from pos, starting at the element iRoot (whose label, inside an object, is
// at iLabel), and returns the index of the element found or a lookup error. With eEdit set it also
// applies the edit there.
func (p *jsonParse) lookupStep(iRoot int, path string, pos int, iLabel int) int {
	if at(path, pos) == 0 {
		if p.eEdit != 0 {
			p.makeEditable()
			n, sz := p.payloadSize(iRoot)
			sz += n
			switch p.eEdit {
			case jeditDel:
				if iLabel > 0 {
					sz += iRoot - iLabel
					iRoot = iLabel
				}
				p.blobEdit(iRoot, sz, nil, 0)
			case jeditIns:
				// It exists already: json_insert() leaves it.
			case jeditAins:
				if path[pos-1] != ']' {
					return lookupNotArray
				}
				p.blobEdit(iRoot, 0, p.aIns, 0)
			default:
				p.blobEdit(iRoot, sz, p.aIns, 0)
			}
		}
		p.iLabel = iLabel
		return iRoot
	}
	switch at(path, pos) {
	case '.':
		rawKey := true
		x := p.aBlob[iRoot]
		pos++
		var key string
		var i int
		if at(path, pos) == '"' {
			start := pos + 1
			for i = 1; at(path, pos+i) != 0 && at(path, pos+i) != '"'; i++ {
				if at(path, pos+i) == '\\' && at(path, pos+i+1) != 0 {
					i++
				}
			}
			key = path[start : pos+i]
			if at(path, pos+i) == 0 {
				return lookupPathError
			}
			i++
			rawKey = strings.IndexByte(key, '\\') < 0
		} else {
			for i = 0; at(path, pos+i) != 0 && at(path, pos+i) != '.' && at(path, pos+i) != '['; i++ {
			}
			key = path[pos : pos+i]
			if len(key) == 0 {
				return lookupPathError
			}
		}
		if x&0x0f != jsonbObject {
			return lookupNotFound
		}
		n, sz := p.payloadSize(iRoot)
		j := iRoot + n
		iEnd := j + sz
		for j < iEnd {
			x := p.aBlob[j] & 0x0f
			if x < jsonbText || x > jsonbTextRaw {
				return lookupError
			}
			n, sz := p.payloadSize(j)
			if n == 0 {
				return lookupError
			}
			k := j + n
			if k+sz >= iEnd {
				return lookupError
			}
			label := string(p.aBlob[k : k+sz])
			rawLabel := x == jsonbText || x == jsonbTextRaw
			if labelCompare(key, rawKey, label, rawLabel) {
				v := k + sz
				if p.aBlob[v]&0x0f > jsonbObject {
					return lookupError
				}
				n, sz := p.payloadSize(v)
				if n == 0 || v+n+sz > iEnd {
					return lookupError
				}
				p.iDepth++
				if p.iDepth >= jsonMaxDepth {
					return lookupTooDeep
				}
				rc := p.lookupStep(v, path, pos+i, j)
				p.iDepth--
				if p.delta != 0 {
					p.afterEditSizeAdjust(iRoot)
				}
				return rc
			}
			j = k + sz
			if p.aBlob[j]&0x0f > jsonbObject {
				return lookupError
			}
			n, sz = p.payloadSize(j)
			if n == 0 {
				return lookupError
			}
			j += n + sz
		}
		if j > iEnd {
			return lookupError
		}
		if p.eEdit >= jeditIns {
			if p.eEdit == jeditAins && !strings.HasSuffix(beforeNUL(path[min(pos+i, len(path)):]), "]") {
				return lookupNotArray
			}
			ix := &jsonParse{}
			if rawKey {
				ix.appendNode(jsonbTextRaw, len(key), nil)
			} else {
				ix.appendNode(jsonbText5, len(key), nil)
			}
			v, rc := p.createEditSubstructure(path, pos+i)
			if !lookupIsError(rc) {
				p.makeEditable()
				ins := append(append(append([]byte(nil), ix.aBlob...), key...), v.aBlob...)
				p.blobEdit(j, 0, nil, len(ins))
				copy(p.aBlob[j:], ins)
				if p.delta != 0 {
					p.afterEditSizeAdjust(iRoot)
				}
			}
			return rc
		}
	case '[':
		x := p.aBlob[iRoot] & 0x0f
		if x != jsonbArray {
			return lookupNotFound
		}
		n, sz := p.payloadSize(iRoot)
		var kk uint64
		i := 1
		for isDigitByte(at(path, pos+i)) {
			if kk < 0xffffffff {
				kk = kk*10 + uint64(at(path, pos+i)-'0')
			}
			i++
		}
		if i < 2 || at(path, pos+i) != ']' {
			if at(path, pos+1) != '#' {
				return lookupPathError
			}
			kk = uint64(p.arrayCount(iRoot))
			i = 2
			if at(path, pos+2) == '-' && isDigitByte(at(path, pos+3)) {
				var nn uint64
				i = 3
				for {
					if nn < 0xffffffff {
						nn = nn*10 + uint64(at(path, pos+i)-'0')
					}
					i++
					if !isDigitByte(at(path, pos+i)) {
						break
					}
				}
				if nn > kk {
					return lookupNotFound
				}
				kk -= nn
			}
			if at(path, pos+i) != ']' {
				return lookupPathError
			}
		}
		j := iRoot + n
		iEnd := j + sz
		for j < iEnd {
			if kk == 0 {
				p.iDepth++
				if p.iDepth >= jsonMaxDepth {
					return lookupTooDeep
				}
				rc := p.lookupStep(j, path, pos+i+1, 0)
				p.iDepth--
				if p.delta != 0 {
					p.afterEditSizeAdjust(iRoot)
				}
				return rc
			}
			kk--
			n, sz := p.payloadSize(j)
			if n == 0 {
				return lookupError
			}
			j += n + sz
		}
		if j > iEnd {
			return lookupError
		}
		if kk > 0 {
			return lookupNotFound
		}
		if p.eEdit >= jeditIns {
			v, rc := p.createEditSubstructure(path, pos+i+1)
			if !lookupIsError(rc) {
				p.makeEditable()
				p.blobEdit(j, 0, v.aBlob, 0)
			}
			if p.delta != 0 {
				p.afterEditSizeAdjust(iRoot)
			}
			return rc
		}
	default:
		return lookupPathError
	}
	return lookupNotFound
}

// returnFromBlob is the element at i as an SQL value: a primitive as itself; an array or object as
// JSON text (eMode 1) or JSONB (eMode 2).
func (p *jsonParse) returnFromBlob(i int, eMode int) (any, error) {
	n, sz := p.payloadSize(i)
	if n == 0 {
		return nil, errMalformedJSON
	}
	a := p.aBlob
	switch a[i] & 0x0f {
	case jsonbNull:
		if sz != 0 {
			return nil, errMalformedJSON
		}
		return nil, nil
	case jsonbTrue:
		if sz != 0 {
			return nil, errMalformedJSON
		}
		return int64(1), nil
	case jsonbFalse:
		if sz != 0 {
			return nil, errMalformedJSON
		}
		return int64(0), nil
	case jsonbInt5, jsonbInt:
		if sz == 0 {
			return nil, errMalformedJSON
		}
		neg := false
		if a[i+n] == '-' {
			if sz < 2 {
				return nil, errMalformedJSON
			}
			n++
			sz--
			neg = true
		}
		res, rc := decOrHexToI64(beforeNUL(string(a[i+n : i+n+sz])))
		switch {
		case rc == 0:
			if res < 0 {
				// 16 hex digits with the top bit set: positive in JSON, so a REAL.
				r := float64(uint64(res))
				if neg {
					return -r, nil
				}
				return r, nil
			}
			if neg {
				return -res, nil
			}
			return res, nil
		case rc == 3 && neg:
			return int64(math.MinInt64), nil
		case rc == 1:
			return nil, errMalformedJSON
		}
		if neg {
			n--
			sz++
		}
		return toDouble(a[i+n : i+n+sz])
	case jsonbFloat5, jsonbFloat:
		if sz == 0 {
			return nil, errMalformedJSON
		}
		return toDouble(a[i+n : i+n+sz])
	case jsonbTextRaw, jsonbText:
		return string(a[i+n : i+n+sz]), nil
	case jsonbText5, jsonbTextJ:
		z := string(a[i+n : i+n+sz])
		out := make([]byte, 0, sz)
		for iIn := 0; iIn < sz; iIn++ {
			c := z[iIn]
			if c != '\\' {
				out = append(out, c)
				continue
			}
			szEscape, v := jsonUnescapeOneChar(z[iIn:])
			switch {
			case v <= 0x7f:
				out = append(out, byte(v))
			case v <= 0x7ff:
				out = append(out, byte(0xc0|v>>6), byte(0x80|v&0x3f))
			case v < 0x10000:
				out = append(out, byte(0xe0|v>>12), byte(0x80|(v>>6)&0x3f), byte(0x80|v&0x3f))
			case v == jsonInvalidChar:
				// Malformed escapes are dropped.
			default:
				out = append(out, byte(0xf0|v>>18), byte(0x80|(v>>12)&0x3f), byte(0x80|(v>>6)&0x3f), byte(0x80|v&0x3f))
			}
			iIn += szEscape - 1
		}
		return string(out), nil
	case jsonbArray, jsonbObject:
		if eMode == 2 {
			return append([]byte(nil), a[i:i+n+sz]...), nil
		}
		x := &jsonParse{aBlob: a[i : i+n+sz]}
		var s jsonString
		x.translateBlobToText(0, &s)
		return s.result(false)
	}
	return nil, errMalformedJSON
}

func toDouble(b []byte) (any, error) {
	r, ok := sqliteAtoF(beforeNUL(string(b)))
	if !ok {
		return nil, errMalformedJSON
	}
	return r, nil
}

// decOrHexToI64 is sqlite3DecOrHexToI64: 0 fits, 1 trailing text, 2 too large, 3 exactly 2^63.
func decOrHexToI64(z string) (int64, int) {
	if at(z, 0) == '0' && (at(z, 1) == 'x' || at(z, 1) == 'X') {
		var u uint64
		i := 2
		for at(z, i) == '0' {
			i++
		}
		k := i
		for ; sqlIsXdigit(at(z, k)); k++ {
			u = u*16 + uint64(jsonHexToInt(at(z, k)))
		}
		switch {
		case k-i > 16:
			return int64(u), 2
		case at(z, k) != 0:
			return int64(u), 1
		}
		return int64(u), 0
	}
	n := 0
	for n < len(z) && strings.IndexByte("+- \n\t0123456789", z[n]) >= 0 {
		n++
	}
	if n < len(z) {
		n++
	}
	return atoi64(z[:n])
}

// atoi64 is sqlite3Atoi64 over all of z.
func atoi64(z string) (int64, int) {
	i := 0
	for i < len(z) && isSpaceByte(z[i]) {
		i++
	}
	neg := false
	if i < len(z) {
		if z[i] == '-' {
			neg = true
			i++
		} else if z[i] == '+' {
			i++
		}
	}
	start := i
	for i < len(z) && z[i] == '0' {
		i++
	}
	digitsStart := i
	var u uint64
	for i < len(z) && isDigitByte(z[i]) {
		u = u*10 + uint64(z[i]-'0')
		i++
	}
	nd := i - digitsStart
	var v int64
	switch {
	case u > math.MaxInt64:
		if neg {
			v = math.MinInt64
		} else {
			v = math.MaxInt64
		}
	case neg:
		v = -int64(u)
	default:
		v = int64(u)
	}
	rc := 0
	if nd == 0 && start == digitsStart {
		rc = -1
	} else if i < len(z) {
		for jj := i; jj < len(z); jj++ {
			if !isSpaceByte(z[jj]) {
				rc = 1
				break
			}
		}
	}
	if nd < 19 {
		return v, rc
	}
	c := 1
	if nd == 19 {
		c = strings.Compare(z[digitsStart:digitsStart+19], "9223372036854775808")
	}
	if c < 0 {
		return v, rc
	}
	if neg {
		v = math.MinInt64
	} else {
		v = math.MaxInt64
	}
	if c > 0 {
		return v, 2
	}
	if neg {
		return v, rc
	}
	return v, 3
}

// jsonArgIsJsonb is a BLOB argument as JSONB, when it is one: its outermost element must span the
// blob exactly, and a blob of seven payload bytes or fewer must pass a full check, since it could
// equally be JSON text cast to a blob.
func jsonArgIsJsonb(v any) (*jsonParse, bool) {
	c, x := norm(v)
	if c != classBlob {
		return nil, false
	}
	p := &jsonParse{aBlob: x.([]byte)}
	if p.nBlob() == 0 {
		return nil, false
	}
	b0 := p.aBlob[0]
	if b0&0x0f > jsonbObject {
		return nil, false
	}
	n, sz := p.payloadSize(0)
	if n == 0 || sz+n != p.nBlob() {
		return nil, false
	}
	if b0&0x0f <= jsonbFalse && sz != 0 {
		return nil, false
	}
	if sz > 7 || (b0 != 0x7b && b0 != 0x5b && !isDigitByte(b0)) || p.validityCheck(0, p.nBlob(), 1) == 0 {
		return p, true
	}
	return nil, false
}

// jsonFunctionArgToBlob is a value to insert as JSONB: JSON-subtyped text and JSONB as JSON, other
// values as the JSON they stand for.
func jsonFunctionArgToBlob(v any) (*jsonParse, error) {
	p := &jsonParse{}
	c, x := norm(v)
	switch c {
	case classNull:
		p.aBlob = []byte{0x00}
	case classBlob:
		px, ok := jsonArgIsJsonb(v)
		if !ok {
			return nil, errJSONBlob
		}
		return px, nil
	case classText:
		s := x.(string)
		if _, isJSON := v.(jsonText); isJSON {
			p.zJson = s
			p.hasJson = true
			if !p.convertTextToBlob() {
				return nil, errMalformedJSON
			}
		} else {
			p.appendNode(jsonbTextRaw, len(s), []byte(s))
		}
	case classReal:
		f := x.(float64)
		switch z := realText(f); {
		case math.IsNaN(f):
			p.appendNode(jsonbNull, 0, nil)
		case z[0] == 'I':
			p.appendNode(jsonbFloat, 5, []byte("9e999"))
		case z[0] == '-' && z[1] == 'I':
			p.appendNode(jsonbFloat, 6, []byte("-9e999"))
		default:
			p.appendNode(jsonbFloat, len(z), []byte(z))
		}
	case classInteger:
		z := sqlText(x)
		p.appendNode(jsonbInt, len(z), []byte(z))
	}
	return p, nil
}

const (
	jsonEditable  = 0x01
	jsonKeepError = 0x02
)

// jsonParseFuncArg is a function's JSON argument parsed: JSONB as itself, anything else as JSON text.
// nil with no error for NULL.
func jsonParseFuncArg(v any, flgs int) (*jsonParse, error) {
	c, _ := norm(v)
	if c == classNull {
		return nil, nil
	}
	if c == classBlob {
		if p, ok := jsonArgIsJsonb(v); ok {
			if flgs&jsonEditable != 0 {
				p.makeEditable()
			}
			return p, nil
		}
		// A blob that is not JSONB is read as JSON text, as SQLite has long done.
	}
	p := &jsonParse{zJson: sqlText(v), hasJson: true, owned: true}
	if len(p.zJson) == 0 || !p.convertTextToBlob() {
		if flgs&jsonKeepError != 0 {
			p.nErr = 1
			return p, nil
		}
		return nil, errMalformedJSON
	}
	return p, nil
}

// returnParse is the parse as a function result: JSON text, or JSONB when asBlob.
func (p *jsonParse) returnParse(asBlob bool) (any, error) {
	if asBlob {
		return append([]byte(nil), p.aBlob...), nil
	}
	var s jsonString
	p.delta = 0
	p.translateBlobToText(0, &s)
	return s.result(false)
}

// sqlQuote is %Q: text in single quotes with quotes doubled.
func sqlQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// jsonBadPathError is the error for a failed lookup of path.
func jsonBadPathError(path string, rc int) error {
	switch rc {
	case lookupNotArray:
		return fmt.Errorf("not an array element: %s", sqlQuote(path))
	case lookupError:
		return errMalformedJSON
	case lookupTooDeep:
		return errors.New("JSON path too deep")
	}
	return fmt.Errorf("bad JSON path: %s", sqlQuote(path))
}
