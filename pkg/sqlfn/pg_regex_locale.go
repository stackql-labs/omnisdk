package sqlfn

// regc_cvec.c, regc_locale.c and regc_pg_locale.c. stackql's database has the C collation and
// ctype, so regc_pg_locale.c takes its PG_REGEX_LOCALE_C strategy: a code point is a digit,
// letter, space and so on only if it is ASCII and C's ctype says so, case maps ASCII letters
// alone, and every character-class cvec ends at 127, with no high-colormap class columns.

// reGetcvec is getcvec: a fresh cvec with room for nchrs chrs and nranges ranges.
func reGetcvec(nchrs, nranges int) *reCvec {
	return &reCvec{chrspace: nchrs, rangespace: nranges, cclasscode: -1}
}

func (cv *reCvec) addchr(c reChr) { cv.chrs = append(cv.chrs, c) }

func (cv *reCvec) addrange(from, to reChr) { cv.ranges = append(cv.ranges, from, to) }

// C-locale character properties (pg_char_properties).
const (
	rePG_ISDIGIT = 0x01
	rePG_ISALPHA = 0x02
	rePG_ISALNUM = rePG_ISDIGIT | rePG_ISALPHA
	rePG_ISUPPER = 0x04
	rePG_ISLOWER = 0x08
	rePG_ISGRAPH = 0x10
	rePG_ISPRINT = 0x20
	rePG_ISPUNCT = 0x40
	rePG_ISSPACE = 0x80
)

// rePgCharProperties is pg_char_properties: C's ctype over ASCII.
var rePgCharProperties = func() [128]byte {
	var t [128]byte
	for c := 0; c < 128; c++ {
		var p byte
		switch {
		case c >= '0' && c <= '9':
			p = rePG_ISDIGIT | rePG_ISGRAPH | rePG_ISPRINT
		case c >= 'A' && c <= 'Z':
			p = rePG_ISALPHA | rePG_ISUPPER | rePG_ISGRAPH | rePG_ISPRINT
		case c >= 'a' && c <= 'z':
			p = rePG_ISALPHA | rePG_ISLOWER | rePG_ISGRAPH | rePG_ISPRINT
		case c == ' ':
			p = rePG_ISPRINT | rePG_ISSPACE
		case c >= '\t' && c <= '\r':
			p = rePG_ISSPACE
		case c > ' ' && c < 0x7f:
			p = rePG_ISGRAPH | rePG_ISPRINT | rePG_ISPUNCT
		}
		t[c] = p
	}
	return t
}()

func rePgWcProp(c reChr, p byte) bool { return c <= 127 && rePgCharProperties[c]&p != 0 }

func rePgWcIsdigit(c reChr) bool { return rePgWcProp(c, rePG_ISDIGIT) }
func rePgWcIsalpha(c reChr) bool { return rePgWcProp(c, rePG_ISALPHA) }
func rePgWcIsalnum(c reChr) bool { return rePgWcProp(c, rePG_ISALNUM) }
func rePgWcIsupper(c reChr) bool { return rePgWcProp(c, rePG_ISUPPER) }
func rePgWcIslower(c reChr) bool { return rePgWcProp(c, rePG_ISLOWER) }
func rePgWcIsgraph(c reChr) bool { return rePgWcProp(c, rePG_ISGRAPH) }
func rePgWcIsprint(c reChr) bool { return rePgWcProp(c, rePG_ISPRINT) }
func rePgWcIspunct(c reChr) bool { return rePgWcProp(c, rePG_ISPUNCT) }
func rePgWcIsspace(c reChr) bool { return rePgWcProp(c, rePG_ISSPACE) }

// rePgWcIsword is pg_wc_isword: alnum plus underscore.
func rePgWcIsword(c reChr) bool { return c == '_' || rePgWcIsalnum(c) }

func rePgWcToupper(c reChr) reChr {
	if c >= 'a' && c <= 'z' {
		return c - 'a' + 'A'
	}
	return c
}

func rePgWcTolower(c reChr) reChr {
	if c >= 'A' && c <= 'Z' {
		return c - 'A' + 'a'
	}
	return c
}

// rePgCtypeGetCache is pg_ctype_get_cache in C locale: the ASCII chrs satisfying probe, runs as
// ranges, and no locale-dependent class code.
func rePgCtypeGetCache(probe func(reChr) bool) *reCvec {
	cv := &reCvec{cclasscode: -1}
	nmatches := 0
	var cur reChr
	for cur = 0; cur <= 127; cur++ {
		if probe(cur) {
			nmatches++
		} else if nmatches > 0 {
			reStoreMatch(cv, cur-reChr(nmatches), nmatches)
			nmatches = 0
		}
	}
	if nmatches > 0 {
		reStoreMatch(cv, cur-reChr(nmatches), nmatches)
	}
	cv.chrspace = len(cv.chrs)
	cv.rangespace = len(cv.ranges) / 2
	return cv
}

func reStoreMatch(cv *reCvec, chr1 reChr, nchrs int) {
	if nchrs > 1 {
		cv.addrange(chr1, chr1+reChr(nchrs)-1)
	} else {
		cv.addchr(chr1)
	}
}

// reCnames is cnames: the collating-element names.
var reCnames = []struct {
	name string
	code byte
}{
	{"NUL", 0}, {"SOH", 0o1}, {"STX", 0o2}, {"ETX", 0o3}, {"EOT", 0o4}, {"ENQ", 0o5}, {"ACK", 0o6},
	{"BEL", 0o7}, {"alert", 0o7}, {"BS", 0o10}, {"backspace", '\b'}, {"HT", 0o11}, {"tab", '\t'},
	{"LF", 0o12}, {"newline", '\n'}, {"VT", 0o13}, {"vertical-tab", '\v'}, {"FF", 0o14},
	{"form-feed", '\f'}, {"CR", 0o15}, {"carriage-return", '\r'}, {"SO", 0o16}, {"SI", 0o17},
	{"DLE", 0o20}, {"DC1", 0o21}, {"DC2", 0o22}, {"DC3", 0o23}, {"DC4", 0o24}, {"NAK", 0o25},
	{"SYN", 0o26}, {"ETB", 0o27}, {"CAN", 0o30}, {"EM", 0o31}, {"SUB", 0o32}, {"ESC", 0o33},
	{"IS4", 0o34}, {"FS", 0o34}, {"IS3", 0o35}, {"GS", 0o35}, {"IS2", 0o36}, {"RS", 0o36},
	{"IS1", 0o37}, {"US", 0o37}, {"space", ' '}, {"exclamation-mark", '!'}, {"quotation-mark", '"'},
	{"number-sign", '#'}, {"dollar-sign", '$'}, {"percent-sign", '%'}, {"ampersand", '&'},
	{"apostrophe", '\''}, {"left-parenthesis", '('}, {"right-parenthesis", ')'}, {"asterisk", '*'},
	{"plus-sign", '+'}, {"comma", ','}, {"hyphen", '-'}, {"hyphen-minus", '-'}, {"period", '.'},
	{"full-stop", '.'}, {"slash", '/'}, {"solidus", '/'}, {"zero", '0'}, {"one", '1'}, {"two", '2'},
	{"three", '3'}, {"four", '4'}, {"five", '5'}, {"six", '6'}, {"seven", '7'}, {"eight", '8'},
	{"nine", '9'}, {"colon", ':'}, {"semicolon", ';'}, {"less-than-sign", '<'}, {"equals-sign", '='},
	{"greater-than-sign", '>'}, {"question-mark", '?'}, {"commercial-at", '@'},
	{"left-square-bracket", '['}, {"backslash", '\\'}, {"reverse-solidus", '\\'},
	{"right-square-bracket", ']'}, {"circumflex", '^'}, {"circumflex-accent", '^'},
	{"underscore", '_'}, {"low-line", '_'}, {"grave-accent", '`'}, {"left-brace", '{'},
	{"left-curly-bracket", '{'}, {"vertical-line", '|'}, {"right-brace", '}'},
	{"right-curly-bracket", '}'}, {"tilde", '~'}, {"DEL", 0o177},
}

// reClassNames is classNames, in enum char_classes order.
var reClassNames = []string{"alnum", "alpha", "ascii", "blank", "cntrl", "digit", "graph",
	"lower", "print", "punct", "space", "upper", "xdigit", "word"}

// reChrsEqual is pg_char_and_wchar_strncmp's equality of an ASCII name and chrs.
func reChrsEqual(name string, chrs []reChr) bool {
	if len(name) != len(chrs) {
		return false
	}
	for i := 0; i < len(name); i++ {
		if reChr(name[i]) != chrs[i] {
			return false
		}
	}
	return true
}

// element maps the collating-element name pat[startp:endp] to its chr.
func (v *reVars) element(startp, endp int) reChr {
	return v.elementChrs(v.pat[startp:endp])
}

func (v *reVars) elementChrs(name []reChr) reChr {
	if len(name) == 1 {
		return name[0]
	}
	v.note(reULOCALE)
	for _, cn := range reCnames {
		if reChrsEqual(cn.name, name) {
			return reChr(cn.code)
		}
	}
	v.seterr(reECOLLATE)
	return 0
}

// rangecvec is range: the cvec for a range, with a legality check.
func (v *reVars) rangecvec(a, b reChr, cases bool) *reCvec {
	if a != b && !(a < b) {
		v.seterr(reERANGE)
		return nil
	}

	if !cases {
		cv := reGetcvec(0, 1)
		cv.addrange(a, b)
		return cv
	}

	nchrs := int(b) - int(a) + 1
	if nchrs <= 0 || nchrs > 100000 {
		nchrs = 100000
	}

	cv := reGetcvec(nchrs, 1)
	cv.addrange(a, b)

	for c := uint64(a); c <= uint64(b); c++ {
		cc := rePgWcTolower(reChr(c))
		if cc != reChr(c) && (cc < a || b < cc) {
			if len(cv.chrs) >= cv.chrspace {
				v.seterr(reETOOBIG)
				return nil
			}
			cv.addchr(cc)
		}
		cc = rePgWcToupper(reChr(c))
		if cc != reChr(c) && (cc < a || b < cc) {
			if len(cv.chrs) >= cv.chrspace {
				v.seterr(reETOOBIG)
				return nil
			}
			cv.addchr(cc)
		}
	}
	return cv
}

// eclass is the cvec for an equivalence class, which in Postgres is the chr alone (with its case
// counterparts on request).
func (v *reVars) eclass(c reChr, cases bool) *reCvec {
	if cases {
		return v.allcases(c)
	}
	cv := reGetcvec(1, 0)
	cv.addchr(c)
	return cv
}

// lookupcclass looks up the character class named pat[startp:endp].
func (v *reVars) lookupcclass(startp, endp int) int {
	name := v.pat[startp:endp]
	for i, cn := range reClassNames {
		if reChrsEqual(cn, name) {
			return i
		}
	}
	v.seterr(reECTYPE)
	return 0
}

// cclasscvec is the cvec for a character class.
func (v *reVars) cclasscvec(cclasscode int, cases bool) *reCvec {
	if cases && (cclasscode == reCC_LOWER || cclasscode == reCC_UPPER) {
		cclasscode = reCC_ALPHA
	}
	var cv *reCvec
	switch cclasscode {
	case reCC_PRINT:
		cv = rePgCtypeGetCache(rePgWcIsprint)
	case reCC_ALNUM:
		cv = rePgCtypeGetCache(rePgWcIsalnum)
	case reCC_ALPHA:
		cv = rePgCtypeGetCache(rePgWcIsalpha)
	case reCC_WORD:
		cv = rePgCtypeGetCache(rePgWcIsword)
	case reCC_ASCII:
		cv = reGetcvec(0, 1)
		cv.addrange(0, 0x7f)
	case reCC_BLANK:
		cv = reGetcvec(2, 0)
		cv.addchr('\t')
		cv.addchr(' ')
	case reCC_CNTRL:
		cv = reGetcvec(0, 2)
		cv.addrange(0x0, 0x1f)
		cv.addrange(0x7f, 0x9f)
	case reCC_DIGIT:
		cv = rePgCtypeGetCache(rePgWcIsdigit)
	case reCC_PUNCT:
		cv = rePgCtypeGetCache(rePgWcIspunct)
	case reCC_XDIGIT:
		cv = reGetcvec(0, 3)
		cv.addrange('0', '9')
		cv.addrange('a', 'f')
		cv.addrange('A', 'F')
	case reCC_SPACE:
		cv = rePgCtypeGetCache(rePgWcIsspace)
	case reCC_LOWER:
		cv = rePgCtypeGetCache(rePgWcIslower)
	case reCC_UPPER:
		cv = rePgCtypeGetCache(rePgWcIsupper)
	case reCC_GRAPH:
		cv = rePgCtypeGetCache(rePgWcIsgraph)
	}
	if cv == nil {
		v.seterr(reESPACE)
	}
	return cv
}

// cclassColumnIndex is cclass_column_index: the high colormap column for a chr.
func (cm *reColormap) cclassColumnIndex(c reChr) int {
	colnum := 0
	probe := func(cls int, f func(reChr) bool) {
		if cm.classbits[cls] != 0 && f(c) {
			colnum |= cm.classbits[cls]
		}
	}
	probe(reCC_PRINT, rePgWcIsprint)
	probe(reCC_ALNUM, rePgWcIsalnum)
	probe(reCC_ALPHA, rePgWcIsalpha)
	probe(reCC_WORD, rePgWcIsword)
	probe(reCC_DIGIT, rePgWcIsdigit)
	probe(reCC_PUNCT, rePgWcIspunct)
	probe(reCC_SPACE, rePgWcIsspace)
	probe(reCC_LOWER, rePgWcIslower)
	probe(reCC_UPPER, rePgWcIsupper)
	probe(reCC_GRAPH, rePgWcIsgraph)
	return colnum
}

// allcases is the cvec for all case counterparts of a chr, itself included.
func (v *reVars) allcases(c reChr) *reCvec {
	lc := rePgWcTolower(c)
	uc := rePgWcToupper(c)
	cv := reGetcvec(2, 0)
	cv.addchr(lc)
	if lc != uc {
		cv.addchr(uc)
	}
	return cv
}

// reCmp is cmp, or casecmp under icase: whether two equal-length chr strings differ.
func reCmp(icase bool, x, y []reChr) bool {
	for i := range x {
		if x[i] != y[i] && (!icase || rePgWcTolower(x[i]) != rePgWcTolower(y[i])) {
			return true
		}
	}
	return false
}
