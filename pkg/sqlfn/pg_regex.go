package sqlfn

// Postgres's regular expression engine: Henry Spencer's ARE package as Postgres 14.5 ships it in
// src/backend/regex, ported over code points (pg_wchar) with the C collation's character classes.
// The structure follows the C closely, file by file: this file is regcomp.c with regex.h and
// regguts.h; pg_regex_lex.go is regc_lex.c, pg_regex_color.go regc_color.c, pg_regex_nfa.go
// regc_nfa.c, pg_regex_locale.go regc_cvec.c, regc_locale.c and regc_pg_locale.c, pg_regex_exec.go
// regexec.c and pg_regex_dfa.go rege_dfa.c. A C pointer into the subject string is an index, and a
// NULL one -1.

// reChr is chr: a pg_wchar, here a Unicode code point.
type reChr = uint32

// reColor is color: an equivalence class of chrs.
type reColor = int

// Compile flags (regex.h).
const (
	reBASIC    = 0o0
	reEXTENDED = 0o1
	reADVF     = 0o2
	reADVANCED = 0o3
	reQUOTE    = 0o4
	reICASE    = 0o10
	reNOSUB    = 0o20
	reEXPANDED = 0o40
	reNLSTOP   = 0o100
	reNLANCH   = 0o200
	reNEWLINE  = 0o300
	reEXPECT   = 0o1000
	reBOSONLY  = 0o2000
)

// Execution flags.
const (
	reNOTBOL = 0o1
	reNOTEOL = 0o2
)

// Error codes.
const (
	reOKAY     = 0
	reNOMATCH  = 1
	reBADPAT   = 2
	reECOLLATE = 3
	reECTYPE   = 4
	reEESCAPE  = 5
	reESUBREG  = 6
	reEBRACK   = 7
	reEPAREN   = 8
	reEBRACE   = 9
	reBADBR    = 10
	reERANGE   = 11
	reESPACE   = 12
	reBADRPT   = 13
	reASSERT   = 15
	reINVARG   = 16
	reMIXED    = 17
	reBADOPT   = 18
	reETOOBIG  = 19
	reECOLORS  = 20
	reCANCEL   = 21
)

// reErrors is regerrs.h: each code's explanation, as pg_regerror reports it.
var reErrors = map[int]string{
	reOKAY:     "no errors detected",
	reNOMATCH:  "failed to match",
	reBADPAT:   "invalid regexp (reg version 0.8)",
	reECOLLATE: "invalid collating element",
	reECTYPE:   "invalid character class",
	reEESCAPE:  "invalid escape \\ sequence",
	reESUBREG:  "invalid backreference number",
	reEBRACK:   "brackets [] not balanced",
	reEPAREN:   "parentheses () not balanced",
	reEBRACE:   "braces {} not balanced",
	reBADBR:    "invalid repetition count(s)",
	reERANGE:   "invalid character range",
	reESPACE:   "out of memory",
	reBADRPT:   "quantifier operand invalid",
	reASSERT:   "\"cannot happen\" -- you found a bug",
	reINVARG:   "invalid argument to regex function",
	reMIXED:    "character widths of regex and string differ",
	reBADOPT:   "invalid embedded option",
	reETOOBIG:  "regular expression is too complex",
	reECOLORS:  "too many colors",
	reCANCEL:   "operation cancelled",
}

// reErrorText is pg_regerror for a real error code.
func reErrorText(code int) string {
	if s, ok := reErrors[code]; ok {
		return s
	}
	return "*** unknown regex error code 0x" + reHex(code) + " ***"
}

func reHex(n int) string {
	const digits = "0123456789abcdef"
	if n == 0 {
		return "0"
	}
	u := uint32(n)
	var b []byte
	for u > 0 {
		b = append([]byte{digits[u%16]}, b...)
		u /= 16
	}
	return string(b)
}

// re_info bits.
const (
	reUBACKREF    = 0o1
	reULOOKAROUND = 0o2
	reUBOUNDS     = 0o4
	reUBRACES     = 0o10
	reUBSALNUM    = 0o20
	reUPBOTCH     = 0o40
	reUBBS        = 0o100
	reUNONPOSIX   = 0o200
	reUUNSPEC     = 0o400
	reUUNPORT     = 0o1000
	reULOCALE     = 0o2000
	reUEMPTYMATCH = 0o4000
	reUIMPOSSIBLE = 0o10000
	reUSHORTEST   = 0o20000
)

// regguts.h and regcustom.h.
const (
	reCHR_MAX        = 0x7ffffffe
	reMAX_SIMPLE_CHR = 0x7FF
	reDUPMAX         = 255
	reDUPINF         = reDUPMAX + 1

	reLATYPE_AHEAD_POS  = 0o3
	reLATYPE_AHEAD_NEG  = 0o2
	reLATYPE_BEHIND_POS = 0o1
	reLATYPE_BEHIND_NEG = 0o0

	reMAX_COLOR = 32767
	reCOLORLESS = -1
	reRAINBOW   = -2
	reWHITE     = 0

	reNOSUBCOLOR = reCOLORLESS
	reFREECOL    = 0o1
	rePSEUDO     = 0o2
	reCOLMARK    = 0o4
	reNINLINECDS = 10

	reFREESTATE = -1

	reHASLACONS = 0o1
	reMATCHALL  = 0o2

	reCNFA_NOPROGRESS = 0o1

	reFIRSTABSIZE = 64
	reMAXABSIZE   = 1024
	reFIRSTSBSIZE = 32
	reMAXSBSIZE   = 1024

	// The sizes, on a 64-bit build, that REG_MAX_COMPILE_SPACE is counted in.
	reSizeofState       = 56
	reSizeofArc         = 72
	reSizeofBatchHead   = 16
	reMAX_COMPILE_SPACE = 500000 * (reSizeofState + 4*reSizeofArc)
)

func reLATYPE_IS_POS(la int) bool   { return la&0o1 != 0 }
func reLATYPE_IS_AHEAD(la int) bool { return la&0o2 != 0 }

// reMaxStackDepth is max_stack_depth's default, 2MB, the depth past which stack_is_too_deep fails
// the regex code's recursions with REG_ETOOBIG.
const reMaxStackDepth = 2048 * 1024

// reFrames are the bytes of stack the regex functions take in a postgres:14.5-bullseye build, read
// from its binary: each function's frame (on x86_64 with its return address, and with the
// arguments pushed for its callers when it takes more than six), and what the check takes from
// its caller's frame to the address it probes. Functions the compiler inlined into their callers
// (parseqatom, delsub, dupnfa, removeconstraints, breakconstraintloop, find, cfind, cfindloop, the
// dissect helpers, lacon, matchuntil) take none of their own.
type reFrames struct {
	regcomp, parse, parsebranch, subre, repeat                       int
	deltraverse, duptraverse, cleartraverse, removetraverse          int
	nfatree, nfanode, optimize, cleanup, markreachable, markcanreach int
	emptyreachable, findconstraintloop, clonesuccessorstates         int
	checkmatchall                                                    int
	regexec, cdissect, longest, shortest, miss                       int
	check                                                            int
}

// reFramesAMD64 and reFramesARM64 are the x86_64 and aarch64 builds' frames.
var (
	reFramesAMD64 = reFrames{regcomp: 336, parse: 96, parsebranch: 160, subre: 80, repeat: 80,
		deltraverse: 48, duptraverse: 48, cleartraverse: 32, removetraverse: 64,
		nfatree: 112, nfanode: 80, optimize: 176, cleanup: 32, markreachable: 48, markcanreach: 48,
		emptyreachable: 48, findconstraintloop: 80, clonesuccessorstates: 128 + 16, checkmatchall: 80,
		regexec: 34768, cdissect: 144, longest: 112, shortest: 112 + 16, miss: 224, check: 41}
	reFramesARM64 = reFrames{regcomp: 336, parse: 96, parsebranch: 176, subre: 64, repeat: 80,
		deltraverse: 64, duptraverse: 48, cleartraverse: 48, removetraverse: 80,
		nfatree: 112, nfanode: 64, optimize: 160, cleanup: 32, markreachable: 48, markcanreach: 64,
		emptyreachable: 48, findconstraintloop: 80, clonesuccessorstates: 112, checkmatchall: 80,
		regexec: 34784, cdissect: 128, longest: 112, shortest: 128, miss: 208, check: 25}
)

// reStack is the server's stack as the regex code uses it: the bytes in use from its base.
type reStack struct {
	fr   *reFrames
	used int
}

func (s *reStack) push(n int) { s.used += n }

func (s *reStack) pop(n int) { s.used -= n }

// tooDeep is STACK_TOO_DEEP: stack_is_too_deep called from the current function.
func (s *reStack) tooDeep() bool { return s.used+s.fr.check > reMaxStackDepth }

// Character classes (enum char_classes).
const (
	reCC_ALNUM = iota
	reCC_ALPHA
	reCC_ASCII
	reCC_BLANK
	reCC_CNTRL
	reCC_DIGIT
	reCC_GRAPH
	reCC_LOWER
	reCC_PRINT
	reCC_PUNCT
	reCC_SPACE
	reCC_UPPER
	reCC_XDIGIT
	reCC_WORD
	reNUM_CCLASSES
)

// Subre flags.
const (
	reLONGER  = 0o1
	reSHORTER = 0o2
	reMIXED_F = 0o4
	reCAP     = 0o10
	reBACKR   = 0o20
	reINUSE   = 0o100
	reNOPROP  = 0o3
)

func reLMIX(f int) int  { return f << 2 }
func reSMIX(f int) int  { return f << 1 }
func reUP(f int) int    { return (f &^ reNOPROP) | (reLMIX(f) & reSMIX(f) & reMIXED_F) }
func reMESSY(f int) int { return f & (reMIXED_F | reCAP | reBACKR) }
func rePREF(f int) int  { return f & reNOPROP }
func rePREF2(f1, f2 int) int {
	if rePREF(f1) != 0 {
		return rePREF(f1)
	}
	return rePREF(f2)
}
func reCOMBINE(f1, f2 int) int { return reUP(f1|f2) | rePREF2(f1, f2) }

// Token type codes, some also NFA arc types.
const (
	reEMPTY   = 'n'
	reEOS     = 'e'
	rePLAIN   = 'p'
	reDIGIT   = 'd'
	reBACKREF = 'b'
	reCOLLEL  = 'I'
	reECLASS  = 'E'
	reCCLASS  = 'C'
	reEND     = 'X'
	reCCLASSS = 's'
	reCCLASSC = 'c'
	reRANGE   = 'R'
	reLACON   = 'L'
	reAHEAD   = 'a'
	reBEHIND  = 'r'
	reWBDRY   = 'w'
	reNWBDRY  = 'W'
	reSBEGIN  = 'A'
	reSEND    = 'Z'
)

// reCOLORED is COLORED: whether an arc belongs on a color chain.
func reCOLORED(a *reArc) bool {
	return a.co >= 0 && (a.typ == rePLAIN || a.typ == reAHEAD || a.typ == reBEHIND)
}

// reColordesc is struct colordesc.
type reColordesc struct {
	nschrs   int
	nuchrs   int
	sub      reColor
	arcs     *reArc
	firstchr reChr
	flags    int
}

// reCmrange is colormaprange.
type reCmrange struct {
	cmin, cmax reChr
	rownum     int
}

// reColormap is struct colormap.
type reColormap struct {
	v            *reVars
	max          int
	free         reColor
	cd           []reColordesc
	locolormap   []reColor
	classbits    [reNUM_CCLASSES]int
	cmranges     []reCmrange
	hicolormap   []reColor
	maxarrayrows int
	hiarrayrows  int
	hiarraycols  int
}

// getcolor is GETCOLOR.
func (cm *reColormap) getcolor(c reChr) reColor {
	if c <= reMAX_SIMPLE_CHR {
		return cm.locolormap[c]
	}
	return cm.getcolorhi(c)
}

// reCvec is struct cvec.
type reCvec struct {
	chrs       []reChr
	chrspace   int
	ranges     []reChr
	rangespace int
	cclasscode int
}

// reArc is struct arc.
type reArc struct {
	typ           int
	co            reColor
	from, to      *reState
	outchain      *reArc // also the free chain
	outchainRev   *reArc
	inchain       *reArc
	inchainRev    *reArc
	colorchain    *reArc
	colorchainRev *reArc
}

// reState is struct state.
type reState struct {
	no    int
	flag  byte
	nins  int
	nouts int
	ins   *reArc
	outs  *reArc
	tmp   *reState
	next  *reState
	prev  *reState
}

// reNFA is struct nfa. Its states and arcs are counted against the compile space in the batches
// regc_nfa.c allocates them in.
type reNFA struct {
	pre, init, final, post *reState
	nstates                int
	states, slast          *reState
	freestates             *reState
	freearcs               *reArc
	sbsizes                []int
	lastsbused             int
	absizes                []int
	lastabused             int
	cm                     *reColormap
	bos, eos               [2]reColor
	flags                  int
	minmatchall            int
	maxmatchall            int
	v                      *reVars
	parent                 *reNFA
}

// reCarc is struct carc.
type reCarc struct {
	co reColor
	to int
}

// reCnfa is struct cnfa: states[n] is state n's outarcs, ending in a COLORLESS arc.
type reCnfa struct {
	nstates     int
	ncolors     int
	flags       int
	pre, post   int
	bos, eos    [2]reColor
	stflags     []byte
	states      [][]reCarc
	minmatchall int
	maxmatchall int
}

// reSubre is struct subre.
type reSubre struct {
	op      byte
	flags   int
	latype  int
	id      int
	capno   int
	backno  int
	min     int
	max     int
	child   *reSubre
	sibling *reSubre
	begin   *reState
	end     *reState
	cnfa    reCnfa
}

// reRegex is a compiled RE: regex_t with its guts.
type reRegex struct {
	info   int
	nsub   int
	cflags int
	tree   *reSubre
	search reCnfa
	ntree  int
	cmap   reColormap
	icase  bool
	lacons []reSubre
}

// reVars is regcomp.c's struct vars.
type reVars struct {
	re        *reRegex
	pat       []reChr
	now       int
	stop      int
	err       int
	cflags    int
	lasttype  int
	nexttype  int
	nextvalue reChr
	lexcon    int
	nsubexp   int
	subs      []*reSubre
	nfa       *reNFA
	cm        *reColormap
	nlcolor   reColor
	wordchrs  *reState
	tree      *reSubre
	ntree     int
	lacons    []reSubre
	nlacons   int
	spaceused int
	stk       reStack
}

func (v *reVars) see(t int) bool { return v.nexttype == t }

func (v *reVars) eat(t int) bool { return v.see(t) && v.next() != 0 }

func (v *reVars) iserr() bool { return v.err != 0 }

// seterr is ERR: it records the first error and ends the token stream.
func (v *reVars) seterr(e int) {
	v.nexttype = reEOS
	if v.err == 0 {
		v.err = e
	}
}

func (v *reVars) insist(c bool, e int) {
	if !c {
		v.seterr(e)
	}
}

func (v *reVars) note(b int) { v.re.info |= b }

func (v *reVars) emptyarc(x, y *reState) { v.nfa.newarc(reEMPTY, 0, x, y) }

// reCompile is pg_regcomp: pattern compiled under flags, or the error code, called with stk in use.
func reCompile(pattern []reChr, flags int, stk reStack) (*reRegex, int) {
	if flags&reQUOTE != 0 && flags&(reADVANCED|reEXPANDED|reNEWLINE) != 0 {
		return nil, reINVARG
	}
	if flags&reEXTENDED == 0 && flags&reADVF != 0 {
		return nil, reINVARG
	}
	re := &reRegex{}
	v := &reVars{re: re, pat: pattern, stop: len(pattern), cflags: flags, subs: make([]*reSubre, 10),
		nlcolor: reCOLORLESS, stk: stk}
	v.stk.push(v.stk.fr.regcomp)
	v.initcm(&re.cmap)
	v.cm = &re.cmap
	v.nfa = v.newnfa(v.cm, nil)
	if v.iserr() {
		return nil, v.err
	}

	v.lexstart()
	if v.cflags&reNLSTOP != 0 || v.cflags&reNLANCH != 0 {
		v.nlcolor = v.cm.subcolor('\n')
		v.cm.okcolors(v.nfa)
	}
	if v.iserr() {
		return nil, v.err
	}
	v.tree = v.parse(reEOS, rePLAIN, v.nfa.init, v.nfa.final)
	if v.iserr() {
		return nil, v.err
	}

	v.nfa.specialcolors()
	if v.iserr() {
		return nil, v.err
	}
	v.ntree = reNumst(v.tree, 1)
	reMarkst(v.tree)

	re.info |= v.nfatree(v.tree)
	if v.iserr() {
		return nil, v.err
	}
	for i := 1; i < v.nlacons; i++ {
		lasub := &v.lacons[i]
		v.nfanode(lasub, !reLATYPE_IS_AHEAD(lasub.latype))
	}
	if v.iserr() {
		return nil, v.err
	}
	if v.tree.flags&reSHORTER != 0 {
		v.note(reUSHORTEST)
	}

	v.nfa.optimize()
	if v.iserr() {
		return nil, v.err
	}
	v.makesearch(v.nfa)
	if v.iserr() {
		return nil, v.err
	}
	v.nfa.compact(&re.search)
	if v.iserr() {
		return nil, v.err
	}

	re.nsub = v.nsubexp
	re.cflags = v.cflags
	re.tree = v.tree
	re.ntree = v.ntree
	re.icase = v.cflags&reICASE != 0
	re.lacons = v.lacons
	v.nfa.freenfa()
	return re, reOKAY
}

// moresubs enlarges the subRE vector.
func (v *reVars) moresubs(wanted int) {
	n := wanted*3/2 + 1
	p := make([]*reSubre, n)
	copy(p, v.subs)
	v.subs = p
}

// makesearch turns an NFA into a search NFA (implicit prepend of .*?).
func (v *reVars) makesearch(nfa *reNFA) {
	pre := nfa.pre
	var a, b *reArc

	for a = pre.outs; a != nil; a = a.outchain {
		if a.co != nfa.bos[0] && a.co != nfa.bos[1] {
			break
		}
	}
	if a != nil {
		nfa.rainbow(v.cm, rePLAIN, reCOLORLESS, pre, pre)
		nfa.newarc(rePLAIN, nfa.bos[0], pre, pre)
		nfa.newarc(rePLAIN, nfa.bos[1], pre, pre)
		if nfa.flags&reMATCHALL != 0 {
			nfa.maxmatchall = reDUPINF
		}
	}

	var slist *reState
	for a = pre.outs; a != nil; a = a.outchain {
		s := a.to
		for b = s.ins; b != nil; b = b.inchain {
			if b.from != pre {
				break
			}
		}
		if b != nil && s.tmp == nil {
			if slist != nil {
				s.tmp = slist
			} else {
				s.tmp = s
			}
			slist = s
		}
	}

	var s2 *reState
	for s := slist; s != nil; s = s2 {
		s2 = nfa.newstate()
		if v.iserr() {
			return
		}
		nfa.copyouts(s, s2)
		if v.iserr() {
			return
		}
		for a = s.ins; a != nil; a = b {
			b = a.inchain
			if a.from != pre {
				nfa.cparc(a, a.from, s2)
				nfa.freearc(a)
			}
		}
		if s.tmp != s {
			s2 = s.tmp
		} else {
			s2 = nil
		}
		s.tmp = nil
	}
}

// parse parses an RE: branches tied together with '|'.
func (v *reVars) parse(stopper, typ int, init, final *reState) *reSubre {
	v.stk.push(v.stk.fr.parse)
	defer v.stk.pop(v.stk.fr.parse)
	branches := v.subre('|', reLONGER, init, final)
	if v.iserr() {
		return nil
	}
	var lastbranch *reSubre
	for {
		left := v.nfa.newstate()
		right := v.nfa.newstate()
		if v.iserr() {
			return nil
		}
		v.emptyarc(init, left)
		v.emptyarc(right, final)
		if v.iserr() {
			return nil
		}
		branch := v.parsebranch(stopper, typ, left, right, false)
		if v.iserr() {
			return nil
		}
		if lastbranch != nil {
			lastbranch.sibling = branch
		} else {
			branches.child = branch
		}
		branches.flags |= reUP(branches.flags | branch.flags)
		lastbranch = branch
		if !v.eat('|') {
			break
		}
	}

	if !v.see(stopper) {
		v.seterr(reEPAREN)
	}

	if lastbranch == branches.child {
		v.freesrnode(branches)
		branches = lastbranch
	} else if reMESSY(branches.flags) == 0 {
		v.freesubreandsiblings(branches.child)
		branches.child = nil
		branches.op = '='
	}
	return branches
}

// parsebranch parses one branch of an RE.
func (v *reVars) parsebranch(stopper, typ int, left, right *reState, partial bool) *reSubre {
	v.stk.push(v.stk.fr.parsebranch)
	defer v.stk.pop(v.stk.fr.parsebranch)
	lp := left
	seencontent := false
	t := v.subre('=', 0, left, right)
	if v.iserr() {
		return nil
	}
	for !v.see('|') && !v.see(stopper) && !v.see(reEOS) {
		if seencontent {
			lp = v.nfa.newstate()
			if v.iserr() {
				return nil
			}
			v.nfa.moveins(right, lp)
		}
		seencontent = true

		t = v.parseqatom(stopper, typ, lp, right, t)
		if v.iserr() {
			return nil
		}
	}

	if !seencontent {
		if !partial {
			v.note(reUUNSPEC)
		}
		v.emptyarc(left, right)
	}
	return t
}

// parseqatom parses one quantified atom or constraint of an RE.
func (v *reVars) parseqatom(stopper, typ int, lp, rp *reState, top *reSubre) *reSubre {
	var s, s2 *reState
	arcv := func(t int, val reColor) { v.nfa.newarc(t, val, lp, rp) }
	var m, n int
	var atom, t *reSubre
	var subno int
	var qprefer int
	var f int
	var atomp **reSubre

	atomtype := v.nexttype
	switch atomtype {
	case '^':
		arcv('^', 1)
		if v.cflags&reNLANCH != 0 {
			arcv(reBEHIND, v.nlcolor)
		}
		v.next()
		return top
	case '$':
		arcv('$', 1)
		if v.cflags&reNLANCH != 0 {
			arcv(reAHEAD, v.nlcolor)
		}
		v.next()
		return top
	case reSBEGIN:
		arcv('^', 1)
		arcv('^', 0)
		v.next()
		return top
	case reSEND:
		arcv('$', 1)
		arcv('$', 0)
		v.next()
		return top
	case '<':
		v.wordchrsSetup()
		s = v.nfa.newstate()
		if v.iserr() {
			return nil
		}
		v.nonword(reBEHIND, lp, s)
		v.word(reAHEAD, s, rp)
		v.next()
		return top
	case '>':
		v.wordchrsSetup()
		s = v.nfa.newstate()
		if v.iserr() {
			return nil
		}
		v.word(reBEHIND, lp, s)
		v.nonword(reAHEAD, s, rp)
		v.next()
		return top
	case reWBDRY:
		v.wordchrsSetup()
		s = v.nfa.newstate()
		if v.iserr() {
			return nil
		}
		v.nonword(reBEHIND, lp, s)
		v.word(reAHEAD, s, rp)
		s = v.nfa.newstate()
		if v.iserr() {
			return nil
		}
		v.word(reBEHIND, lp, s)
		v.nonword(reAHEAD, s, rp)
		v.next()
		return top
	case reNWBDRY:
		v.wordchrsSetup()
		s = v.nfa.newstate()
		if v.iserr() {
			return nil
		}
		v.word(reBEHIND, lp, s)
		v.word(reAHEAD, s, rp)
		s = v.nfa.newstate()
		if v.iserr() {
			return nil
		}
		v.nonword(reBEHIND, lp, s)
		v.nonword(reAHEAD, s, rp)
		v.next()
		return top
	case reLACON:
		latype := int(v.nextvalue)
		v.next()
		s = v.nfa.newstate()
		s2 = v.nfa.newstate()
		if v.iserr() {
			return nil
		}
		t = v.parse(')', reLACON, s, s2)
		v.freesubre(t)
		if v.iserr() {
			return nil
		}
		v.next()
		v.processlacon(s, s2, latype, lp, rp)
		return top
	case '*', '+', '?', '{':
		v.seterr(reBADRPT)
		return top
	case ')', rePLAIN:
		if atomtype == ')' {
			if v.cflags&reADVANCED != reEXTENDED {
				v.seterr(reEPAREN)
				return top
			}
			v.note(reUPBOTCH)
		}
		v.onechr(v.nextvalue, lp, rp)
		v.cm.okcolors(v.nfa)
		if v.iserr() {
			return nil
		}
		v.next()
	case '[':
		if v.nextvalue == 1 {
			v.bracket(lp, rp)
		} else {
			v.cbracket(lp, rp)
		}
		v.next()
	case reCCLASSS:
		v.charclass(int(v.nextvalue), lp, rp)
		v.cm.okcolors(v.nfa)
		v.next()
	case reCCLASSC:
		v.charclasscomplement(int(v.nextvalue), lp, rp)
		v.next()
	case '.':
		but := reColor(reCOLORLESS)
		if v.cflags&reNLSTOP != 0 {
			but = v.nlcolor
		}
		v.nfa.rainbow(v.cm, rePLAIN, but, lp, rp)
		v.next()
	case '(':
		cap := v.nextvalue != 0
		if typ == reLACON {
			cap = false
		}
		if cap {
			v.nsubexp++
			subno = v.nsubexp
			if subno >= len(v.subs) {
				v.moresubs(subno)
			}
		} else {
			atomtype = rePLAIN
		}
		v.next()

		s = v.nfa.newstate()
		s2 = v.nfa.newstate()
		if v.iserr() {
			return nil
		}
		v.emptyarc(lp, s)
		v.emptyarc(s2, rp)
		if v.iserr() {
			return nil
		}
		atom = v.parse(')', typ, s, s2)
		v.next()
		if v.iserr() {
			return nil
		}
		if cap {
			if atom.capno == 0 {
				atom.flags |= reCAP
				atom.capno = subno
			} else {
				t = v.subre('(', atom.flags|reCAP, s, s2)
				if v.iserr() {
					return nil
				}
				t.capno = subno
				t.child = atom
				atom = t
			}
			v.subs[subno] = atom
		}
	case reBACKREF:
		v.insist(typ != reLACON, reESUBREG)
		v.insist(int(v.nextvalue) < len(v.subs), reESUBREG)
		v.insist(int(v.nextvalue) < len(v.subs) && v.subs[v.nextvalue] != nil, reESUBREG)
		if v.iserr() {
			return nil
		}
		atom = v.subre('b', reBACKR, lp, rp)
		if v.iserr() {
			return nil
		}
		subno = int(v.nextvalue)
		atom.backno = subno
		v.emptyarc(lp, rp)
		v.next()
	default:
		v.seterr(reASSERT)
		return top
	}

	// ...and an atom may be followed by a quantifier
	switch v.nexttype {
	case '*':
		m, n = 0, reDUPINF
		qprefer = reSHORTER
		if v.nextvalue != 0 {
			qprefer = reLONGER
		}
		v.next()
	case '+':
		m, n = 1, reDUPINF
		qprefer = reSHORTER
		if v.nextvalue != 0 {
			qprefer = reLONGER
		}
		v.next()
	case '?':
		m, n = 0, 1
		qprefer = reSHORTER
		if v.nextvalue != 0 {
			qprefer = reLONGER
		}
		v.next()
	case '{':
		v.next()
		m = v.scannum()
		if v.eat(',') {
			if v.see(reDIGIT) {
				n = v.scannum()
			} else {
				n = reDUPINF
			}
			if m > n {
				v.seterr(reBADBR)
				return top
			}
			qprefer = reSHORTER
			if v.nextvalue != 0 {
				qprefer = reLONGER
			}
		} else {
			n = m
			qprefer = 0
		}
		if !v.see('}') {
			v.seterr(reBADBR)
			return top
		}
		v.next()
	default:
		m, n = 1, 1
		qprefer = 0
	}

	// annoying special case:  {0} or {0,0} cancels everything
	if m == 0 && n == 0 {
		if atom != nil && atom.flags&reCAP != 0 {
			v.nfa.delsub(lp, atom.begin)
			v.nfa.delsub(atom.end, rp)
		} else {
			if atom != nil {
				v.freesubre(atom)
			}
			v.nfa.delsub(lp, rp)
		}
		v.emptyarc(lp, rp)
		return top
	}

	// if not a messy case, avoid hard part
	f = top.flags | qprefer
	if atom != nil {
		f |= atom.flags
	}
	if atomtype != '(' && atomtype != reBACKREF && reMESSY(reUP(f)) == 0 {
		if !(m == 1 && n == 1) {
			v.repeat(lp, rp, m, n)
		}
		if atom != nil {
			v.freesubre(atom)
		}
		top.flags = f
		return top
	}

	// hard part:  something messy
	if atom == nil {
		atom = v.subre('=', 0, lp, rp)
		if v.iserr() {
			return nil
		}
	}

	if atom.begin == lp || atom.end == rp {
		s = v.nfa.newstate()
		s2 = v.nfa.newstate()
		if v.iserr() {
			return nil
		}
		v.nfa.moveouts(lp, s)
		v.nfa.moveins(rp, s2)
		atom.begin = s
		atom.end = s2
	} else {
		v.nfa.delsub(lp, atom.begin)
		v.nfa.delsub(atom.end, rp)
	}

	s = v.nfa.newstate()
	if v.iserr() {
		return nil
	}
	v.emptyarc(lp, s)
	if v.iserr() {
		return nil
	}

	t = v.subre('.', reCOMBINE(qprefer, atom.flags), lp, rp)
	if v.iserr() {
		return nil
	}
	t.child = atom
	atomp = &t.child

	top.child = v.subre('=', top.flags, top.begin, lp)
	if v.iserr() {
		return nil
	}
	top.op = '.'
	top.child.sibling = t

	if atomtype == reBACKREF {
		v.nfa.delsub(atom.begin, atom.end)
		v.nfa.dupnfa(v.subs[subno].begin, v.subs[subno].end, atom.begin, atom.end)
		if v.iserr() {
			return nil
		}
		v.nfa.removeconstraints(atom.begin, atom.end)
		if v.iserr() {
			return nil
		}
	}

	if atomtype == reBACKREF {
		v.emptyarc(s, atom.begin)
		v.repeat(atom.begin, atom.end, m, n)
		atom.min = m
		atom.max = n
		atom.flags |= reCOMBINE(qprefer, atom.flags)
		s2 = atom.end
	} else if m == 1 && n == 1 &&
		(qprefer == 0 ||
			atom.flags&(reLONGER|reSHORTER|reMIXED_F) == 0 ||
			qprefer == atom.flags&(reLONGER|reSHORTER|reMIXED_F)) {
		v.emptyarc(s, atom.begin)
		s2 = atom.end
	} else if atom.flags&(reCAP|reBACKR) == 0 {
		v.emptyarc(s, atom.begin)
		v.repeat(atom.begin, atom.end, m, n)
		f = reCOMBINE(qprefer, atom.flags)
		t = v.subre('=', f, atom.begin, atom.end)
		if v.iserr() {
			return nil
		}
		v.freesubre(atom)
		*atomp = t
		s2 = t.end
	} else if m > 0 && atom.flags&reBACKR == 0 {
		v.nfa.dupnfa(atom.begin, atom.end, s, atom.begin)
		nn := n
		if n != reDUPINF {
			nn = n - 1
		}
		v.repeat(s, atom.begin, m-1, nn)
		f = reCOMBINE(qprefer, atom.flags)
		t = v.subre('.', f, s, atom.end)
		if v.iserr() {
			return nil
		}
		t.child = v.subre('=', rePREF(f), s, atom.begin)
		if v.iserr() {
			return nil
		}
		t.child.sibling = atom
		*atomp = t
		s2 = atom.end
	} else {
		s2 = v.nfa.newstate()
		if v.iserr() {
			return nil
		}
		v.nfa.moveouts(atom.end, s2)
		if v.iserr() {
			return nil
		}
		v.nfa.dupnfa(atom.begin, atom.end, s, s2)
		v.repeat(s, s2, m, n)
		f = reCOMBINE(qprefer, atom.flags)
		t = v.subre('*', f, s, s2)
		if v.iserr() {
			return nil
		}
		t.min = m
		t.max = n
		t.child = atom
		*atomp = t
	}

	// and finally, look after that postponed recursion
	t = top.child.sibling
	if !(v.see('|') || v.see(stopper) || v.see(reEOS)) {
		t.child.sibling = v.parsebranch(stopper, typ, s2, rp, true)
		if v.iserr() {
			return nil
		}

		t.flags |= reCOMBINE(t.flags, t.child.sibling.flags)
		top.flags |= reCOMBINE(top.flags, t.flags)

		if top.child.begin == top.child.end {
			v.freesubre(top.child)
			top.child = t.child
			v.freesrnode(t)
		} else if t.child.op == '=' && t.child.sibling.op == '=' &&
			reMESSY(reUP(t.child.flags|t.child.sibling.flags)) == 0 {
			t.op = '='
			t.flags = reCOMBINE(t.child.flags, t.child.sibling.flags)
			v.freesubreandsiblings(t.child)
			t.child = nil
		}
	} else {
		v.emptyarc(s2, rp)
		top.child.sibling = t.child
		top.flags |= reCOMBINE(top.flags, top.child.sibling.flags)
		v.freesrnode(t)

		if top.child.begin == top.child.end {
			t = top.child.sibling
			top.child.sibling = nil
			v.freesubre(top)
			top = t
		}
	}
	return top
}

// nonword generates arcs for a non-word character ahead or behind.
func (v *reVars) nonword(dir int, lp, rp *reState) {
	anchor := '^'
	if dir == reAHEAD {
		anchor = '$'
	}
	v.nfa.newarc(int(anchor), 1, lp, rp)
	v.nfa.newarc(int(anchor), 0, lp, rp)
	v.nfa.colorcomplement(v.cm, dir, v.wordchrs, lp, rp)
}

// word generates arcs for a word character ahead or behind.
func (v *reVars) word(dir int, lp, rp *reState) {
	v.nfa.cloneouts(v.wordchrs, lp, rp, dir)
}

// charclass generates arcs for a character class.
func (v *reVars) charclass(cls int, lp, rp *reState) {
	v.note(reULOCALE)
	cv := v.cclasscvec(cls, v.cflags&reICASE != 0)
	if v.iserr() {
		return
	}
	v.subcolorcvec(cv, lp, rp)
}

// charclasscomplement generates arcs for a complemented character class.
func (v *reVars) charclasscomplement(cls int, lp, rp *reState) {
	cstate := v.nfa.newstate()
	if v.iserr() {
		return
	}
	v.note(reULOCALE)
	cv := v.cclasscvec(cls, v.cflags&reICASE != 0)
	if v.iserr() {
		return
	}
	v.subcolorcvec(cv, cstate, cstate)
	if v.iserr() {
		return
	}
	v.cm.okcolors(v.nfa)
	if v.iserr() {
		return
	}
	v.nfa.colorcomplement(v.cm, rePLAIN, cstate, lp, rp)
	if v.iserr() {
		return
	}
	v.nfa.dropstate(cstate)
}

// scannum scans a number, at most DUPMAX.
func (v *reVars) scannum() int {
	n := 0
	for v.see(reDIGIT) && n < reDUPMAX {
		n = n*10 + int(v.nextvalue)
		v.next()
	}
	if v.see(reDIGIT) || n > reDUPMAX {
		v.seterr(reBADBR)
		return 0
	}
	return n
}

// repeat replicates a subNFA for quantifiers.
func (v *reVars) repeat(lp, rp *reState, m, n int) {
	v.stk.push(v.stk.fr.repeat)
	defer v.stk.pop(v.stk.fr.repeat)
	const (
		some = 2
		inf  = 3
	)
	reduce := func(x int) int {
		if x == reDUPINF {
			return inf
		}
		if x > 1 {
			return some
		}
		return x
	}
	pair := func(x, y int) int { return x*4 + y }
	rm := reduce(m)
	rn := reduce(n)
	var s, s2 *reState

	switch pair(rm, rn) {
	case pair(0, 0):
		v.nfa.delsub(lp, rp)
		v.emptyarc(lp, rp)
	case pair(0, 1):
		v.emptyarc(lp, rp)
	case pair(0, some):
		v.repeat(lp, rp, 1, n)
		if v.iserr() {
			return
		}
		v.emptyarc(lp, rp)
	case pair(0, inf):
		s = v.nfa.newstate()
		if v.iserr() {
			return
		}
		v.nfa.moveouts(lp, s)
		v.nfa.moveins(rp, s)
		v.emptyarc(lp, s)
		v.emptyarc(s, rp)
	case pair(1, 1):
	case pair(1, some):
		s = v.nfa.newstate()
		if v.iserr() {
			return
		}
		v.nfa.moveouts(lp, s)
		v.nfa.dupnfa(s, rp, lp, s)
		if v.iserr() {
			return
		}
		v.repeat(lp, s, 1, n-1)
		if v.iserr() {
			return
		}
		v.emptyarc(lp, s)
	case pair(1, inf):
		s = v.nfa.newstate()
		s2 = v.nfa.newstate()
		if v.iserr() {
			return
		}
		v.nfa.moveouts(lp, s)
		v.nfa.moveins(rp, s2)
		v.emptyarc(lp, s)
		v.emptyarc(s2, rp)
		v.emptyarc(s2, s)
	case pair(some, some):
		s = v.nfa.newstate()
		if v.iserr() {
			return
		}
		v.nfa.moveouts(lp, s)
		v.nfa.dupnfa(s, rp, lp, s)
		if v.iserr() {
			return
		}
		v.repeat(lp, s, m-1, n-1)
	case pair(some, inf):
		s = v.nfa.newstate()
		if v.iserr() {
			return
		}
		v.nfa.moveouts(lp, s)
		v.nfa.dupnfa(s, rp, lp, s)
		if v.iserr() {
			return
		}
		v.repeat(lp, s, m-1, n)
	default:
		v.seterr(reASSERT)
	}
}

// bracket handles a non-complemented bracket expression.
func (v *reVars) bracket(lp, rp *reState) {
	var haveCclassc [reNUM_CCLASSES]bool

	v.next()
	for !v.see(']') && !v.see(reEOS) {
		v.brackpart(lp, rp, &haveCclassc)
	}

	v.cm.okcolors(v.nfa)
	if v.iserr() {
		return
	}

	anyCclassc := false
	for i := 0; i < reNUM_CCLASSES; i++ {
		if haveCclassc[i] {
			v.charclasscomplement(i, lp, rp)
			if v.iserr() {
				return
			}
			anyCclassc = true
		}
	}

	if anyCclassc {
		v.optimizebracket(lp, rp)
	}
}

// cbracket handles a complemented bracket expression.
func (v *reVars) cbracket(lp, rp *reState) {
	left := v.nfa.newstate()
	right := v.nfa.newstate()
	if v.iserr() {
		return
	}
	v.bracket(left, right)

	if v.cflags&reNLSTOP != 0 {
		v.nfa.newarc(rePLAIN, v.nlcolor, left, right)
	}
	if v.iserr() {
		return
	}

	v.nfa.colorcomplement(v.cm, rePLAIN, left, lp, rp)
	if v.iserr() {
		return
	}
	v.nfa.dropstate(left)
	v.nfa.freestate(right)
}

// brackpart handles one item (or range) within a bracket expression.
func (v *reVars) brackpart(lp, rp *reState, haveCclassc *[reNUM_CCLASSES]bool) {
	var startc, endc reChr
	var cv *reCvec
	var startp, endp int

	switch v.nexttype {
	case reRANGE:
		v.seterr(reERANGE)
		return
	case rePLAIN:
		startc = v.nextvalue
		v.next()
		if !v.see(reRANGE) {
			v.onechr(startc, lp, rp)
			return
		}
		if v.iserr() {
			return
		}
	case reCOLLEL:
		startp = v.now
		endp = v.scanplain()
		v.insist(startp < endp, reECOLLATE)
		if v.iserr() {
			return
		}
		startc = v.element(startp, endp)
		if v.iserr() {
			return
		}
	case reECLASS:
		startp = v.now
		endp = v.scanplain()
		v.insist(startp < endp, reECOLLATE)
		if v.iserr() {
			return
		}
		startc = v.element(startp, endp)
		if v.iserr() {
			return
		}
		cv = v.eclass(startc, v.cflags&reICASE != 0)
		if v.iserr() {
			return
		}
		v.subcolorcvec(cv, lp, rp)
		return
	case reCCLASS:
		startp = v.now
		endp = v.scanplain()
		v.insist(startp < endp, reECTYPE)
		if v.iserr() {
			return
		}
		cls := v.lookupcclass(startp, endp)
		if v.iserr() {
			return
		}
		v.charclass(cls, lp, rp)
		return
	case reCCLASSS:
		v.charclass(int(v.nextvalue), lp, rp)
		v.next()
		return
	case reCCLASSC:
		haveCclassc[v.nextvalue] = true
		v.next()
		return
	default:
		v.seterr(reASSERT)
		return
	}

	if v.see(reRANGE) {
		v.next()
		switch v.nexttype {
		case rePLAIN, reRANGE:
			endc = v.nextvalue
			v.next()
			if v.iserr() {
				return
			}
		case reCOLLEL:
			startp = v.now
			endp = v.scanplain()
			v.insist(startp < endp, reECOLLATE)
			if v.iserr() {
				return
			}
			endc = v.element(startp, endp)
			if v.iserr() {
				return
			}
		default:
			v.seterr(reERANGE)
			return
		}
	} else {
		endc = startc
	}

	if startc != endc {
		v.note(reUUNPORT)
	}
	cv = v.rangecvec(startc, endc, v.cflags&reICASE != 0)
	if v.iserr() {
		return
	}
	v.subcolorcvec(cv, lp, rp)
}

// scanplain scans the PLAIN contents of [. etc., returning the index just past them.
func (v *reVars) scanplain() int {
	v.next()
	endp := v.now
	for v.see(rePLAIN) {
		endp = v.now
		v.next()
	}
	v.next()
	return endp
}

// onechr fills in arcs for a plain character, and its case complements.
func (v *reVars) onechr(c reChr, lp, rp *reState) {
	if v.cflags&reICASE == 0 {
		lastsubcolor := reColor(reCOLORLESS)
		v.subcoloronechr(c, lp, rp, &lastsubcolor)
		return
	}
	v.subcolorcvec(v.allcases(c), lp, rp)
}

// optimizebracket converts a bracket expression covering every color to a RAINBOW arc.
func (v *reVars) optimizebracket(lp, rp *reState) {
	cm := v.cm
	end := cm.max + 1
	for a := lp.outs; a != nil; a = a.outchain {
		cm.cd[a.co].flags |= reCOLMARK
	}
	israinbow := true
	for co := 0; co < end; co++ {
		cd := &cm.cd[co]
		if cd.flags&reCOLMARK != 0 {
			cd.flags &^= reCOLMARK
		} else if cd.flags&reFREECOL == 0 && cd.flags&rePSEUDO == 0 {
			israinbow = false
		}
	}
	if !israinbow {
		return
	}
	for lp.outs != nil {
		v.nfa.freearc(lp.outs)
	}
	v.nfa.newarc(rePLAIN, reRAINBOW, lp, rp)
}

// wordchrsSetup is wordchrs: it sets up the word-chr list for word-boundary constraints.
func (v *reVars) wordchrsSetup() {
	if v.wordchrs != nil {
		return
	}
	cstate := v.nfa.newstate()
	if v.iserr() {
		return
	}
	v.note(reULOCALE)
	cv := v.cclasscvec(reCC_WORD, v.cflags&reICASE != 0)
	if v.iserr() {
		return
	}
	v.subcolorcvec(cv, cstate, cstate)
	if v.iserr() {
		return
	}
	v.cm.okcolors(v.nfa)
	if v.iserr() {
		return
	}
	v.wordchrs = cstate
}

// processlacon generates the NFA representation of a LACON.
func (v *reVars) processlacon(begin, end *reState, latype int, lp, rp *reState) {
	s1 := reSingleColorTransition(begin, end)
	switch latype {
	case reLATYPE_AHEAD_POS:
		if s1 != nil {
			v.nfa.cloneouts(s1, lp, rp, reAHEAD)
			return
		}
	case reLATYPE_AHEAD_NEG:
		if s1 != nil {
			v.nfa.colorcomplement(v.cm, reAHEAD, s1, lp, rp)
			v.nfa.newarc('$', 1, lp, rp)
			v.nfa.newarc('$', 0, lp, rp)
			return
		}
	case reLATYPE_BEHIND_POS:
		if s1 != nil {
			v.nfa.cloneouts(s1, lp, rp, reBEHIND)
			return
		}
	case reLATYPE_BEHIND_NEG:
		if s1 != nil {
			v.nfa.colorcomplement(v.cm, reBEHIND, s1, lp, rp)
			v.nfa.newarc('^', 1, lp, rp)
			v.nfa.newarc('^', 0, lp, rp)
			return
		}
	}
	n := v.newlacon(begin, end, latype)
	v.nfa.newarc(reLACON, n, lp, rp)
}

// subre allocates a subre. Its stack check is what protects parse and its recursion.
func (v *reVars) subre(op byte, flags int, begin, end *reState) *reSubre {
	v.stk.push(v.stk.fr.subre)
	defer v.stk.pop(v.stk.fr.subre)
	if v.stk.tooDeep() {
		v.seterr(reETOOBIG)
		return nil
	}
	return &reSubre{op: op, flags: flags, latype: -1, min: 1, max: 1, begin: begin, end: end}
}

// freesubre frees a subRE subtree, not its siblings.
func (v *reVars) freesubre(sr *reSubre) {
	if sr == nil {
		return
	}
	if sr.child != nil {
		v.freesubreandsiblings(sr.child)
	}
	v.freesrnode(sr)
}

// freesubreandsiblings frees a subRE subtree and its following siblings.
func (v *reVars) freesubreandsiblings(sr *reSubre) {
	for sr != nil {
		next := sr.sibling
		v.freesubre(sr)
		sr = next
	}
}

// freesrnode frees one node in a subRE subtree.
func (v *reVars) freesrnode(sr *reSubre) {
	if sr == nil {
		return
	}
	sr.cnfa = reCnfa{}
	sr.flags = 0
	sr.child, sr.sibling = nil, nil
	sr.begin, sr.end = nil, nil
}

// reNumst numbers tree nodes, assigning their ids.
func reNumst(t *reSubre, start int) int {
	i := start
	t.id = i
	i++
	for t2 := t.child; t2 != nil; t2 = t2.sibling {
		i = reNumst(t2, i)
	}
	return i
}

// reMarkst marks tree nodes INUSE.
func reMarkst(t *reSubre) {
	t.flags |= reINUSE
	for t2 := t.child; t2 != nil; t2 = t2.sibling {
		reMarkst(t2)
	}
}

// nfatree turns a subRE subtree into a tree of compacted NFAs. The build unrolls one level of its
// recursion, nfanode-ing each child from its own frame and tail-calling nfanode for t.
func (v *reVars) nfatree(t *reSubre) int {
	v.stk.push(v.stk.fr.nfatree)
	for t2 := t.child; t2 != nil; t2 = t2.sibling {
		for t3 := t2.child; t3 != nil; t3 = t3.sibling {
			v.nfatree(t3)
		}
		v.nfanode(t2, false)
	}
	v.stk.pop(v.stk.fr.nfatree)
	return v.nfanode(t, false)
}

// nfanode does one NFA for nfatree or lacons.
func (v *reVars) nfanode(t *reSubre, converttosearch bool) int {
	v.stk.push(v.stk.fr.nfanode)
	defer v.stk.pop(v.stk.fr.nfanode)
	nfa := v.newnfa(v.cm, v.nfa)
	if v.iserr() {
		return 0
	}
	ret := 0
	nfa.dupnfa(t.begin, t.end, nfa.init, nfa.final)
	if !v.iserr() {
		nfa.specialcolors()
	}
	if !v.iserr() {
		ret = nfa.optimize()
	}
	if converttosearch && !v.iserr() {
		v.makesearch(nfa)
	}
	if !v.iserr() {
		nfa.compact(&t.cnfa)
	}
	nfa.freenfa()
	return ret
}

// newlacon allocates a lookaround-constraint subRE.
func (v *reVars) newlacon(begin, end *reState, latype int) int {
	var n int
	if v.nlacons == 0 {
		n = 1
		v.lacons = make([]reSubre, 2)
	} else {
		n = v.nlacons
		v.lacons = append(v.lacons, reSubre{})
	}
	v.nlacons = n + 1
	sub := &v.lacons[n]
	sub.begin = begin
	sub.end = end
	sub.latype = latype
	sub.cnfa = reCnfa{}
	return n
}
