package sqlfn

// regc_lex.c: the regex lexical analyzer.

// Lexical contexts.
const (
	reL_ERE   = 1
	reL_BRE   = 2
	reL_Q     = 3
	reL_EBND  = 4
	reL_BBND  = 5
	reL_BRACK = 6
	reL_CEL   = 7
	reL_ECL   = 8
	reL_CCL   = 9
)

func (v *reVars) ateos() bool     { return v.now >= v.stop }
func (v *reVars) have(n int) bool { return v.stop-v.now >= n }
func (v *reVars) next1(c byte) bool {
	return !v.ateos() && v.pat[v.now] == reChr(c)
}
func (v *reVars) next2(a, b byte) bool {
	return v.have(2) && v.pat[v.now] == reChr(a) && v.pat[v.now+1] == reChr(b)
}
func (v *reVars) next3(a, b, c byte) bool {
	return v.have(3) && v.pat[v.now] == reChr(a) && v.pat[v.now+1] == reChr(b) && v.pat[v.now+2] == reChr(c)
}

// ret is RET: it sets the next token's type.
func (v *reVars) ret(c int) int {
	v.nexttype = c
	return 1
}

// retv is RETV: it sets the next token's type and value.
func (v *reVars) retv(c int, n reChr) int {
	v.nexttype = c
	v.nextvalue = n
	return 1
}

// failw is FAILW.
func (v *reVars) failw(e int) int {
	v.seterr(e)
	return 0
}

func (v *reVars) lasttypeIs(t int) bool { return v.lasttype == t }

// lexstart sets up lexical stuff and scans leading options.
func (v *reVars) lexstart() {
	v.prefixes()
	if v.iserr() {
		return
	}
	if v.cflags&reQUOTE != 0 {
		v.lexcon = reL_Q
	} else if v.cflags&reEXTENDED != 0 {
		v.lexcon = reL_ERE
	} else {
		v.lexcon = reL_BRE
	}
	v.nexttype = reEMPTY
	v.next()
}

// prefixes implements the special prefixes: ***=, ***: and embedded options.
func (v *reVars) prefixes() {
	if v.cflags&reQUOTE != 0 {
		return
	}

	if v.have(4) && v.next3('*', '*', '*') {
		switch v.pat[v.now+3] {
		case '?':
			v.seterr(reBADPAT)
			return
		case '=':
			v.note(reUNONPOSIX)
			v.cflags |= reQUOTE
			v.cflags &^= reADVANCED | reEXPANDED | reNEWLINE
			v.now += 4
			return
		case ':':
			v.note(reUNONPOSIX)
			v.cflags |= reADVANCED
			v.now += 4
		default:
			v.seterr(reBADRPT)
			return
		}
	}

	if v.cflags&reADVANCED != reADVANCED {
		return
	}

	if v.have(3) && v.next2('(', '?') && rePgWcIsalpha(v.pat[v.now+2]) {
		v.note(reUNONPOSIX)
		v.now += 2
		for ; !v.ateos() && rePgWcIsalpha(v.pat[v.now]); v.now++ {
			switch v.pat[v.now] {
			case 'b':
				v.cflags &^= reADVANCED | reQUOTE
			case 'c':
				v.cflags &^= reICASE
			case 'e':
				v.cflags |= reEXTENDED
				v.cflags &^= reADVF | reQUOTE
			case 'i':
				v.cflags |= reICASE
			case 'm', 'n':
				v.cflags |= reNEWLINE
			case 'p':
				v.cflags |= reNLSTOP
				v.cflags &^= reNLANCH
			case 'q':
				v.cflags |= reQUOTE
				v.cflags &^= reADVANCED
			case 's':
				v.cflags &^= reNEWLINE
			case 't':
				v.cflags &^= reEXPANDED
			case 'w':
				v.cflags &^= reNLSTOP
				v.cflags |= reNLANCH
			case 'x':
				v.cflags |= reEXPANDED
			default:
				v.seterr(reBADOPT)
				return
			}
		}
		if !v.next1(')') {
			v.seterr(reBADOPT)
			return
		}
		v.now++
		if v.cflags&reQUOTE != 0 {
			v.cflags &^= reEXPANDED | reNEWLINE
		}
	}
}

// next gets the next token: 1 normally, 0 on failure.
func (v *reVars) next() int {
	if v.iserr() {
		return 0
	}

	v.lasttype = v.nexttype

	if v.nexttype == reEMPTY && v.cflags&reBOSONLY != 0 {
		return v.retv(reSBEGIN, 0)
	}

	if v.cflags&reEXPANDED != 0 {
		switch v.lexcon {
		case reL_ERE, reL_BRE, reL_EBND, reL_BBND:
			v.skip()
		}
	}

	if v.ateos() {
		switch v.lexcon {
		case reL_ERE, reL_BRE, reL_Q:
			return v.ret(reEOS)
		case reL_EBND, reL_BBND:
			return v.failw(reEBRACE)
		case reL_BRACK, reL_CEL, reL_ECL, reL_CCL:
			return v.failw(reEBRACK)
		}
		return v.failw(reASSERT)
	}

	c := v.pat[v.now]
	v.now++

	switch v.lexcon {
	case reL_BRE:
		return v.brenext(c)
	case reL_ERE:
	case reL_Q:
		return v.retv(rePLAIN, c)
	case reL_BBND, reL_EBND:
		switch {
		case c >= '0' && c <= '9':
			return v.retv(reDIGIT, c-'0')
		case c == ',':
			return v.ret(',')
		case c == '}':
			if v.lexcon == reL_EBND {
				v.lexcon = reL_ERE
				if v.cflags&reADVF != 0 && v.next1('?') {
					v.now++
					v.note(reUNONPOSIX)
					return v.retv('}', 0)
				}
				return v.retv('}', 1)
			}
			return v.failw(reBADBR)
		case c == '\\':
			if v.lexcon == reL_BBND && v.next1('}') {
				v.now++
				v.lexcon = reL_BRE
				return v.retv('}', 1)
			}
			return v.failw(reBADBR)
		default:
			return v.failw(reBADBR)
		}
	case reL_BRACK:
		switch c {
		case ']':
			if v.lasttypeIs('[') {
				return v.retv(rePLAIN, c)
			}
			if v.cflags&reEXTENDED != 0 {
				v.lexcon = reL_ERE
			} else {
				v.lexcon = reL_BRE
			}
			return v.ret(']')
		case '\\':
			v.note(reUBBS)
			if v.cflags&reADVF == 0 {
				return v.retv(rePLAIN, c)
			}
			v.note(reUNONPOSIX)
			if v.ateos() {
				return v.failw(reEESCAPE)
			}
			if v.lexescape() == 0 {
				return 0
			}
			switch v.nexttype {
			case rePLAIN, reCCLASSS, reCCLASSC:
				return 1
			}
			return v.failw(reEESCAPE)
		case '-':
			if v.lasttypeIs('[') || v.next1(']') {
				return v.retv(rePLAIN, c)
			}
			return v.retv(reRANGE, c)
		case '[':
			if v.ateos() {
				return v.failw(reEBRACK)
			}
			d := v.pat[v.now]
			v.now++
			switch d {
			case '.':
				v.lexcon = reL_CEL
				return v.ret(reCOLLEL)
			case '=':
				v.lexcon = reL_ECL
				v.note(reULOCALE)
				return v.ret(reECLASS)
			case ':':
				v.lexcon = reL_CCL
				v.note(reULOCALE)
				return v.ret(reCCLASS)
			default:
				v.now--
				return v.retv(rePLAIN, c)
			}
		default:
			return v.retv(rePLAIN, c)
		}
	case reL_CEL:
		if c == '.' && v.next1(']') {
			v.now++
			v.lexcon = reL_BRACK
			return v.retv(reEND, '.')
		}
		return v.retv(rePLAIN, c)
	case reL_ECL:
		if c == '=' && v.next1(']') {
			v.now++
			v.lexcon = reL_BRACK
			return v.retv(reEND, '=')
		}
		return v.retv(rePLAIN, c)
	case reL_CCL:
		if c == ':' && v.next1(']') {
			v.now++
			v.lexcon = reL_BRACK
			return v.retv(reEND, ':')
		}
		return v.retv(rePLAIN, c)
	default:
		return v.failw(reASSERT)
	}

	// EREs and AREs, except for backslashes
	switch c {
	case '|':
		return v.ret('|')
	case '*', '+', '?':
		if v.cflags&reADVF != 0 && v.next1('?') {
			v.now++
			v.note(reUNONPOSIX)
			return v.retv(int(c), 0)
		}
		return v.retv(int(c), 1)
	case '{':
		if v.cflags&reEXPANDED != 0 {
			v.skip()
		}
		if v.ateos() || !rePgWcIsdigit(v.pat[v.now]) {
			v.note(reUBRACES)
			v.note(reUUNSPEC)
			return v.retv(rePLAIN, c)
		}
		v.note(reUBOUNDS)
		v.lexcon = reL_EBND
		return v.ret('{')
	case '(':
		if v.cflags&reADVF != 0 && v.next1('?') {
			v.note(reUNONPOSIX)
			v.now++
			if v.ateos() {
				return v.failw(reBADRPT)
			}
			d := v.pat[v.now]
			v.now++
			switch d {
			case ':':
				return v.retv('(', 0)
			case '#':
				for !v.ateos() && v.pat[v.now] != ')' {
					v.now++
				}
				if !v.ateos() {
					v.now++
				}
				return v.next()
			case '=':
				v.note(reULOOKAROUND)
				return v.retv(reLACON, reLATYPE_AHEAD_POS)
			case '!':
				v.note(reULOOKAROUND)
				return v.retv(reLACON, reLATYPE_AHEAD_NEG)
			case '<':
				if v.ateos() {
					return v.failw(reBADRPT)
				}
				e := v.pat[v.now]
				v.now++
				switch e {
				case '=':
					v.note(reULOOKAROUND)
					return v.retv(reLACON, reLATYPE_BEHIND_POS)
				case '!':
					v.note(reULOOKAROUND)
					return v.retv(reLACON, reLATYPE_BEHIND_NEG)
				default:
					return v.failw(reBADRPT)
				}
			default:
				return v.failw(reBADRPT)
			}
		}
		if v.cflags&reNOSUB != 0 {
			return v.retv('(', 0)
		}
		return v.retv('(', 1)
	case ')':
		if v.lasttypeIs('(') {
			v.note(reUUNSPEC)
		}
		return v.retv(')', c)
	case '[':
		if v.have(6) && v.pat[v.now] == '[' && v.pat[v.now+1] == ':' &&
			(v.pat[v.now+2] == '<' || v.pat[v.now+2] == '>') &&
			v.pat[v.now+3] == ':' && v.pat[v.now+4] == ']' && v.pat[v.now+5] == ']' {
			c = v.pat[v.now+2]
			v.now += 6
			v.note(reUNONPOSIX)
			if c == '<' {
				return v.ret('<')
			}
			return v.ret('>')
		}
		v.lexcon = reL_BRACK
		if v.next1('^') {
			v.now++
			return v.retv('[', 0)
		}
		return v.retv('[', 1)
	case '.':
		return v.ret('.')
	case '^':
		return v.ret('^')
	case '$':
		return v.ret('$')
	case '\\':
		if v.ateos() {
			return v.failw(reEESCAPE)
		}
	default:
		return v.retv(rePLAIN, c)
	}

	// ERE/ARE backslash handling; backslash already eaten
	if v.cflags&reADVF == 0 {
		if rePgWcIsalnum(v.pat[v.now]) {
			v.note(reUBSALNUM)
			v.note(reUUNSPEC)
		}
		c = v.pat[v.now]
		v.now++
		return v.retv(rePLAIN, c)
	}
	return v.lexescape()
}

// lexescape parses an ARE backslash escape (backslash already eaten).
func (v *reVars) lexescape() int {
	c := v.pat[v.now]
	v.now++
	if !rePgWcIsalnum(c) {
		return v.retv(rePLAIN, c)
	}

	v.note(reUNONPOSIX)
	switch c {
	case 'a':
		return v.retv(rePLAIN, v.chrnamed("alert", '\007'))
	case 'A':
		return v.retv(reSBEGIN, 0)
	case 'b':
		return v.retv(rePLAIN, '\b')
	case 'B':
		return v.retv(rePLAIN, '\\')
	case 'c':
		v.note(reUUNPORT)
		if v.ateos() {
			return v.failw(reEESCAPE)
		}
		d := v.pat[v.now]
		v.now++
		return v.retv(rePLAIN, d&0o37)
	case 'd':
		v.note(reULOCALE)
		return v.retv(reCCLASSS, reCC_DIGIT)
	case 'D':
		v.note(reULOCALE)
		return v.retv(reCCLASSC, reCC_DIGIT)
	case 'e':
		v.note(reUUNPORT)
		return v.retv(rePLAIN, v.chrnamed("ESC", '\033'))
	case 'f':
		return v.retv(rePLAIN, '\f')
	case 'm':
		return v.ret('<')
	case 'M':
		return v.ret('>')
	case 'n':
		return v.retv(rePLAIN, '\n')
	case 'r':
		return v.retv(rePLAIN, '\r')
	case 's':
		v.note(reULOCALE)
		return v.retv(reCCLASSS, reCC_SPACE)
	case 'S':
		v.note(reULOCALE)
		return v.retv(reCCLASSC, reCC_SPACE)
	case 't':
		return v.retv(rePLAIN, '\t')
	case 'u':
		c = v.lexdigits(16, 4, 4)
		if v.iserr() || c > reCHR_MAX {
			return v.failw(reEESCAPE)
		}
		return v.retv(rePLAIN, c)
	case 'U':
		c = v.lexdigits(16, 8, 8)
		if v.iserr() || c > reCHR_MAX {
			return v.failw(reEESCAPE)
		}
		return v.retv(rePLAIN, c)
	case 'v':
		return v.retv(rePLAIN, '\v')
	case 'w':
		v.note(reULOCALE)
		return v.retv(reCCLASSS, reCC_WORD)
	case 'W':
		v.note(reULOCALE)
		return v.retv(reCCLASSC, reCC_WORD)
	case 'x':
		v.note(reUUNPORT)
		c = v.lexdigits(16, 1, 255)
		if v.iserr() || c > reCHR_MAX {
			return v.failw(reEESCAPE)
		}
		return v.retv(rePLAIN, c)
	case 'y':
		v.note(reULOCALE)
		return v.retv(reWBDRY, 0)
	case 'Y':
		v.note(reULOCALE)
		return v.retv(reNWBDRY, 0)
	case 'Z':
		return v.retv(reSEND, 0)
	case '1', '2', '3', '4', '5', '6', '7', '8', '9':
		save := v.now
		v.now--
		c = v.lexdigits(10, 1, 255)
		if v.iserr() {
			return v.failw(reEESCAPE)
		}
		if v.now == save || (int32(c) > 0 && int(int32(c)) <= v.nsubexp) {
			v.note(reUBACKREF)
			return v.retv(reBACKREF, c)
		}
		v.now = save
		return v.lexoctal()
	case '0':
		return v.lexoctal()
	default:
		return v.failw(reEESCAPE)
	}
}

// lexoctal is lexescape's octal case, the first digit having been read.
func (v *reVars) lexoctal() int {
	v.note(reUUNPORT)
	v.now--
	c := v.lexdigits(8, 1, 3)
	if v.iserr() {
		return v.failw(reEESCAPE)
	}
	if c > 0xff {
		v.now--
		c >>= 3
	}
	return v.retv(rePLAIN, c)
}

// lexdigits slurps up digits and returns their value, without overflow checks.
func (v *reVars) lexdigits(base, minlen, maxlen int) reChr {
	var n uint32
	ub := uint32(base)
	length := 0
	for length = 0; length < maxlen && !v.ateos(); length++ {
		c := v.pat[v.now]
		v.now++
		d := -1
		switch {
		case c >= '0' && c <= '9':
			d = int(c - '0')
		case c >= 'a' && c <= 'f':
			d = int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			d = int(c-'A') + 10
		default:
			v.now--
		}
		if d >= base {
			v.now--
			d = -1
		}
		if d < 0 {
			break
		}
		n = n*ub + uint32(d)
	}
	if length < minlen {
		v.seterr(reEESCAPE)
	}
	return n
}

// brenext gets the next BRE token.
func (v *reVars) brenext(c reChr) int {
	switch c {
	case '*':
		if v.lasttypeIs(reEMPTY) || v.lasttypeIs('(') || v.lasttypeIs('^') {
			return v.retv(rePLAIN, c)
		}
		return v.retv('*', 1)
	case '[':
		if v.have(6) && v.pat[v.now] == '[' && v.pat[v.now+1] == ':' &&
			(v.pat[v.now+2] == '<' || v.pat[v.now+2] == '>') &&
			v.pat[v.now+3] == ':' && v.pat[v.now+4] == ']' && v.pat[v.now+5] == ']' {
			c = v.pat[v.now+2]
			v.now += 6
			v.note(reUNONPOSIX)
			if c == '<' {
				return v.ret('<')
			}
			return v.ret('>')
		}
		v.lexcon = reL_BRACK
		if v.next1('^') {
			v.now++
			return v.retv('[', 0)
		}
		return v.retv('[', 1)
	case '.':
		return v.ret('.')
	case '^':
		if v.lasttypeIs(reEMPTY) {
			return v.ret('^')
		}
		if v.lasttypeIs('(') {
			v.note(reUUNSPEC)
			return v.ret('^')
		}
		return v.retv(rePLAIN, c)
	case '$':
		if v.cflags&reEXPANDED != 0 {
			v.skip()
		}
		if v.ateos() {
			return v.ret('$')
		}
		if v.next2('\\', ')') {
			v.note(reUUNSPEC)
			return v.ret('$')
		}
		return v.retv(rePLAIN, c)
	case '\\':
	default:
		return v.retv(rePLAIN, c)
	}

	if v.ateos() {
		return v.failw(reEESCAPE)
	}

	c = v.pat[v.now]
	v.now++
	switch c {
	case '{':
		v.lexcon = reL_BBND
		v.note(reUBOUNDS)
		return v.ret('{')
	case '(':
		return v.retv('(', 1)
	case ')':
		return v.retv(')', c)
	case '<':
		v.note(reUNONPOSIX)
		return v.ret('<')
	case '>':
		v.note(reUNONPOSIX)
		return v.ret('>')
	case '1', '2', '3', '4', '5', '6', '7', '8', '9':
		v.note(reUBACKREF)
		return v.retv(reBACKREF, c-'0')
	default:
		if rePgWcIsalnum(c) {
			v.note(reUBSALNUM)
			v.note(reUUNSPEC)
		}
		return v.retv(rePLAIN, c)
	}
}

// skip skips white space and comments in expanded form.
func (v *reVars) skip() {
	start := v.now
	for {
		for !v.ateos() && rePgWcIsspace(v.pat[v.now]) {
			v.now++
		}
		if v.ateos() || v.pat[v.now] != '#' {
			break
		}
		for !v.ateos() && v.pat[v.now] != '\n' {
			v.now++
		}
	}
	if v.now != start {
		v.note(reUNONPOSIX)
	}
}

// chrnamed is the chr known by a name, or lastresort.
func (v *reVars) chrnamed(name string, lastresort reChr) reChr {
	errsave := v.err
	v.err = 0
	nexttype := v.nexttype
	c := v.elementChrs([]reChr(reWide(name)))
	e := v.err
	v.err = errsave
	if e != 0 {
		v.nexttype = nexttype
		return lastresort
	}
	cv := v.rangecvec(c, c, false)
	if len(cv.chrs) == 0 {
		return lastresort
	}
	return cv.chrs[0]
}
