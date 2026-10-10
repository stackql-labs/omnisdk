package sqlfn

// rege_dfa.c: the lazy-DFA matching engines.

// reArcp is struct arcp: a "pointer" to an outarc.
type reArcp struct {
	ss *reSset
	co reColor
}

// reSset is struct sset: a state set.
type reSset struct {
	states   []uint32
	hash     uint32
	flags    int
	ins      reArcp
	lastseen int
	outs     []*reSset
	inchain  []reArcp
}

// Sset flags.
const (
	reSTARTER    = 0o1
	rePOSTSTATE  = 0o2
	reLOCKED     = 0o4
	reNOPROGRESS = 0o10
)

// reDFA is struct dfa.
type reDFA struct {
	nssets     int
	nssused    int
	nstates    int
	ncolors    int
	wordsper   int
	ssets      []reSset
	statesarea []uint32
	work       []uint32
	outsarea   []*reSset
	incarea    []reArcp
	cnfa       *reCnfa
	cm         *reColormap
	lastpost   int
	lastnopr   int
	search     int
	backno     int
	backmin    int
	backmax    int
}

func reBset(uv []uint32, sn int) { uv[sn/32] |= 1 << (sn % 32) }

func reIsbset(uv []uint32, sn int) bool { return uv[sn/32]&(1<<(sn%32)) != 0 }

// longest is the longest-preferred matching engine: the match end, or -1.
func (v *reExec) longest(d *reDFA, start, stop int, hitstopp *bool) int {
	v.stk.push(v.stk.fr.longest)
	defer v.stk.pop(v.stk.fr.longest)
	realstop := stop
	if stop != v.stop {
		realstop = stop + 1
	}
	cm := d.cm

	if hitstopp != nil {
		*hitstopp = false
	}

	if d.backno >= 0 {
		if v.pmatch[d.backno].so >= 0 {
			cp := v.dfaBackref(d, start, start, stop, false)
			if cp == v.stop && stop == v.stop && hitstopp != nil {
				*hitstopp = true
			}
			return cp
		}
	}

	if d.cnfa.flags&reMATCHALL != 0 {
		nchr := stop - start
		maxmatchall := d.cnfa.maxmatchall
		if nchr < d.cnfa.minmatchall {
			return -1
		}
		if maxmatchall == reDUPINF {
			if stop == v.stop && hitstopp != nil {
				*hitstopp = true
			}
		} else {
			if stop == v.stop && nchr <= maxmatchall+1 && hitstopp != nil {
				*hitstopp = true
			}
			if nchr > maxmatchall {
				return start + maxmatchall
			}
		}
		return stop
	}

	css := v.initialize(d, start)
	if css == nil {
		return -1
	}
	cp := start

	var co reColor
	if cp == v.start {
		if v.eflags&reNOTBOL != 0 {
			co = d.cnfa.bos[0]
		} else {
			co = d.cnfa.bos[1]
		}
	} else {
		co = cm.getcolor(v.data[cp-1])
	}
	css = v.miss(d, css, co, cp, start)
	if css == nil {
		return -1
	}
	css.lastseen = cp

	var ss *reSset
	for cp < realstop {
		co = cm.getcolor(v.data[cp])
		ss = css.outs[co]
		if ss == nil {
			ss = v.miss(d, css, co, cp+1, start)
			if ss == nil {
				break
			}
		}
		cp++
		ss.lastseen = cp
		css = ss
	}

	if v.iserr() {
		return -1
	}

	if cp == v.stop && stop == v.stop {
		if hitstopp != nil {
			*hitstopp = true
		}
		if v.eflags&reNOTEOL != 0 {
			co = d.cnfa.eos[0]
		} else {
			co = d.cnfa.eos[1]
		}
		ss = v.miss(d, css, co, cp, start)
		if v.iserr() {
			return -1
		}
		if ss != nil && ss.flags&rePOSTSTATE != 0 {
			return cp
		} else if ss != nil {
			ss.lastseen = cp
		}
	}

	post := d.lastpost
	for i := 0; i < d.nssused; i++ {
		ss = &d.ssets[i]
		if ss.flags&rePOSTSTATE != 0 && post != ss.lastseen && (post == -1 || post < ss.lastseen) {
			post = ss.lastseen
		}
	}
	if post != -1 {
		return post - 1
	}
	return -1
}

// shortest is the shortest-preferred matching engine: the match end, or -1.
func (v *reExec) shortest(d *reDFA, start, min, max int, coldp *int, hitstopp *bool) int {
	v.stk.push(v.stk.fr.shortest)
	defer v.stk.pop(v.stk.fr.shortest)
	realmin := min
	if min != v.stop {
		realmin = min + 1
	}
	realmax := max
	if max != v.stop {
		realmax = max + 1
	}
	cm := d.cm

	if coldp != nil {
		*coldp = -1
	}
	if hitstopp != nil {
		*hitstopp = false
	}

	if d.backno >= 0 {
		if v.pmatch[d.backno].so >= 0 {
			cp := v.dfaBackref(d, start, min, max, true)
			if cp != -1 && coldp != nil {
				*coldp = start
			}
			return cp
		}
	}

	if d.cnfa.flags&reMATCHALL != 0 {
		nchr := min - start
		if d.cnfa.maxmatchall != reDUPINF && nchr > d.cnfa.maxmatchall {
			return -1
		}
		if max-start < d.cnfa.minmatchall {
			return -1
		}
		if nchr < d.cnfa.minmatchall {
			min = start + d.cnfa.minmatchall
		}
		if coldp != nil {
			*coldp = start
		}
		return min
	}

	css := v.initialize(d, start)
	if css == nil {
		return -1
	}
	cp := start

	var co reColor
	if cp == v.start {
		if v.eflags&reNOTBOL != 0 {
			co = d.cnfa.bos[0]
		} else {
			co = d.cnfa.bos[1]
		}
	} else {
		co = cm.getcolor(v.data[cp-1])
	}
	css = v.miss(d, css, co, cp, start)
	if css == nil {
		return -1
	}
	css.lastseen = cp
	ss := css

	for cp < realmax {
		co = cm.getcolor(v.data[cp])
		ss = css.outs[co]
		if ss == nil {
			ss = v.miss(d, css, co, cp+1, start)
			if ss == nil {
				break
			}
		}
		cp++
		ss.lastseen = cp
		css = ss
		if ss.flags&rePOSTSTATE != 0 && cp >= realmin {
			break
		}
	}

	if ss == nil {
		return -1
	}

	if coldp != nil {
		*coldp = v.lastcold(d)
	}

	if ss.flags&rePOSTSTATE != 0 && cp > min {
		cp--
	} else if cp == v.stop && max == v.stop {
		if v.eflags&reNOTEOL != 0 {
			co = d.cnfa.eos[0]
		} else {
			co = d.cnfa.eos[1]
		}
		ss = v.miss(d, css, co, cp, start)
		if (ss == nil || ss.flags&rePOSTSTATE == 0) && hitstopp != nil {
			*hitstopp = true
		}
	}

	if ss == nil || ss.flags&rePOSTSTATE == 0 {
		return -1
	}
	return cp
}

// matchuntil is the incremental matching engine: whether a match from v.start ends at probe.
func (v *reExec) matchuntil(d *reDFA, probe int, lastcss **reSset, lastcp *int) bool {
	cp := *lastcp
	css := *lastcss
	cm := d.cm

	if d.cnfa.flags&reMATCHALL != 0 {
		nchr := probe - v.start
		return nchr >= d.cnfa.minmatchall
	}

	var co reColor
	if cp == -1 || cp > probe {
		cp = v.start
		css = v.initialize(d, cp)
		if css == nil {
			return false
		}
		if v.eflags&reNOTBOL != 0 {
			co = d.cnfa.bos[0]
		} else {
			co = d.cnfa.bos[1]
		}
		css = v.miss(d, css, co, cp, v.start)
		if css == nil {
			return false
		}
		css.lastseen = cp
	} else if css == nil {
		return false
	}
	ss := css

	for cp < probe {
		co = cm.getcolor(v.data[cp])
		ss = css.outs[co]
		if ss == nil {
			ss = v.miss(d, css, co, cp+1, v.start)
			if ss == nil {
				break
			}
		}
		cp++
		ss.lastseen = cp
		css = ss
	}

	*lastcss = ss
	*lastcp = cp

	if ss == nil {
		return false
	}

	if cp < v.stop {
		co = cm.getcolor(v.data[cp])
		ss = css.outs[co]
		if ss == nil {
			ss = v.miss(d, css, co, cp+1, v.start)
		}
	} else {
		if v.eflags&reNOTEOL != 0 {
			co = d.cnfa.eos[0]
		} else {
			co = d.cnfa.eos[1]
		}
		ss = v.miss(d, css, co, cp, v.start)
	}

	return ss != nil && ss.flags&rePOSTSTATE != 0
}

// dfaBackref finds the best match length for a known backref string.
func (v *reExec) dfaBackref(d *reDFA, start, min, max int, shortest bool) int {
	n := d.backno
	backmin := d.backmin
	backmax := d.backmax

	if v.pmatch[n].so == -1 {
		return -1
	}
	brstring := v.pmatch[n].so
	brlen := v.pmatch[n].eo - v.pmatch[n].so

	if brlen == 0 {
		if min == start && backmin <= backmax {
			return start
		}
		return -1
	}

	var minreps int
	if min <= start {
		minreps = 0
	} else {
		minreps = (min-start-1)/brlen + 1
	}
	maxreps := (max - start) / brlen

	if minreps < backmin {
		minreps = backmin
	}
	if backmax != reDUPINF && maxreps > backmax {
		maxreps = backmax
	}
	if maxreps < minreps {
		return -1
	}

	if shortest && minreps == 0 {
		return start
	}

	p := start
	numreps := 0
	for numreps < maxreps {
		if reCmp(v.re.icase, v.data[brstring:brstring+brlen], v.data[p:p+brlen]) {
			break
		}
		p += brlen
		numreps++
		if shortest && numreps >= minreps {
			break
		}
	}

	if numreps >= minreps {
		return p
	}
	return -1
}

// lastcold determines the last point at which no progress had been made.
func (v *reExec) lastcold(d *reDFA) int {
	nopr := d.lastnopr
	if nopr == -1 {
		nopr = v.start
	}
	for i := 0; i < d.nssused; i++ {
		ss := &d.ssets[i]
		if ss.flags&reNOPROGRESS != 0 && nopr < ss.lastseen {
			nopr = ss.lastseen
		}
	}
	return nopr
}

// newdfa sets up a fresh DFA.
func (v *reExec) newdfa(cnfa *reCnfa, cm *reColormap) *reDFA {
	nss := cnfa.nstates * 2
	wordsper := (cnfa.nstates + 31) / 32
	d := &reDFA{}
	d.ssets = make([]reSset, nss)
	d.statesarea = make([]uint32, (nss+1)*wordsper)
	d.work = d.statesarea[nss*wordsper:]
	d.outsarea = make([]*reSset, nss*cnfa.ncolors)
	d.incarea = make([]reArcp, nss*cnfa.ncolors)

	d.nssets = nss
	d.nssused = 0
	d.nstates = cnfa.nstates
	d.ncolors = cnfa.ncolors
	d.wordsper = wordsper
	d.cnfa = cnfa
	d.cm = cm
	d.lastpost = -1
	d.lastnopr = -1
	d.search = 0
	d.backno = -1
	d.backmin, d.backmax = 0, 0
	return d
}

// reHash is hash: a hash code for a bitvector.
func reHash(uv []uint32, n int) uint32 {
	var h uint32
	for i := 0; i < n; i++ {
		h ^= uv[i]
	}
	return h
}

// initialize hand-crafts a cache entry for startup, otherwise gets ready.
func (v *reExec) initialize(d *reDFA, start int) *reSset {
	var ss *reSset
	if d.nssused > 0 && d.ssets[0].flags&reSTARTER != 0 {
		ss = &d.ssets[0]
	} else {
		ss = v.getvacant(d, start, start)
		if ss == nil {
			return nil
		}
		for i := 0; i < d.wordsper; i++ {
			ss.states[i] = 0
		}
		reBset(ss.states, d.cnfa.pre)
		ss.hash = reHash(ss.states, d.wordsper)
		ss.flags = reSTARTER | reLOCKED | reNOPROGRESS
	}

	for i := 0; i < d.nssused; i++ {
		d.ssets[i].lastseen = -1
	}
	ss.lastseen = start
	d.lastpost = -1
	d.lastnopr = -1
	return ss
}

// miss handles a stateset cache miss: the stateset after consuming a co chr, or nil.
func (v *reExec) miss(d *reDFA, css *reSset, co reColor, cp, start int) *reSset {
	v.stk.push(v.stk.fr.miss)
	defer v.stk.pop(v.stk.fr.miss)
	cnfa := d.cnfa

	if css.outs[co] != nil {
		return css.outs[co]
	}

	for i := 0; i < d.wordsper; i++ {
		d.work[i] = 0
	}
	ispseudocolor := d.cm.cd[co].flags&rePSEUDO != 0
	ispost := false
	noprogress := true
	gotstate := false
	for i := 0; i < d.nstates; i++ {
		if reIsbset(css.states, i) {
			for _, ca := range cnfa.states[i] {
				if ca.co == reCOLORLESS {
					break
				}
				if ca.co == co || (ca.co == reRAINBOW && !ispseudocolor) {
					reBset(d.work, ca.to)
					gotstate = true
					if ca.to == cnfa.post {
						ispost = true
					}
					if cnfa.stflags[ca.to]&reCNFA_NOPROGRESS == 0 {
						noprogress = false
					}
				}
			}
		}
	}
	if !gotstate {
		return nil
	}
	dolacons := cnfa.flags&reHASLACONS != 0
	sawlacons := false
	for dolacons {
		dolacons = false
		for i := 0; i < d.nstates; i++ {
			if reIsbset(d.work, i) {
				for _, ca := range cnfa.states[i] {
					if ca.co == reCOLORLESS {
						break
					}
					if ca.co < cnfa.ncolors {
						continue
					}
					if reIsbset(d.work, ca.to) {
						continue
					}
					sawlacons = true
					if !v.lacon(cnfa, cp, ca.co) {
						if v.iserr() {
							return nil
						}
						continue
					}
					if v.iserr() {
						return nil
					}
					reBset(d.work, ca.to)
					dolacons = true
					if ca.to == cnfa.post {
						ispost = true
					}
					if cnfa.stflags[ca.to]&reCNFA_NOPROGRESS == 0 {
						noprogress = false
					}
				}
			}
		}
	}
	h := reHash(d.work, d.wordsper)

	var p *reSset
	for j := 0; j < d.nssused; j++ {
		q := &d.ssets[j]
		if q.hash == h && reWordsEqual(d.work[:d.wordsper], q.states) {
			p = q
			break
		}
	}
	if p == nil {
		p = v.getvacant(d, cp, start)
		if p == nil {
			return nil
		}
		copy(p.states, d.work[:d.wordsper])
		p.hash = h
		p.flags = 0
		if ispost {
			p.flags = rePOSTSTATE
		}
		if noprogress {
			p.flags |= reNOPROGRESS
		}
	}

	if !sawlacons {
		css.outs[co] = p
		css.inchain[co] = p.ins
		p.ins.ss = css
		p.ins.co = co
	}
	return p
}

func reWordsEqual(a, b []uint32) bool {
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// lacon checks a lookaround constraint for miss.
func (v *reExec) lacon(pcnfa *reCnfa, cp int, co reColor) bool {
	if v.stk.tooDeep() {
		v.seterr(reETOOBIG)
		return false
	}
	n := co - pcnfa.ncolors
	sub := &v.re.lacons[n]
	d := v.getladfa(n)
	if d == nil {
		return false
	}
	var satisfied bool
	if reLATYPE_IS_AHEAD(sub.latype) {
		end := v.shortest(d, cp, cp, v.stop, nil, nil)
		if reLATYPE_IS_POS(sub.latype) {
			satisfied = end != -1
		} else {
			satisfied = end == -1
		}
	} else {
		satisfied = v.matchuntil(d, cp, &v.lblastcss[n], &v.lblastcp[n])
		if !reLATYPE_IS_POS(sub.latype) {
			satisfied = !satisfied
		}
	}
	return satisfied
}

// getvacant gets a vacant state set, clearing its inarcs and outarcs.
func (v *reExec) getvacant(d *reDFA, cp, start int) *reSset {
	ss := v.pickss(d, cp, start)
	if ss == nil {
		return nil
	}

	ap := ss.ins
	for ap.ss != nil {
		p := ap.ss
		co := ap.co
		p.outs[co] = nil
		ap = p.inchain[co]
		p.inchain[co].ss = nil
	}
	ss.ins.ss = nil

	for i := 0; i < d.ncolors; i++ {
		p := ss.outs[i]
		if p == nil {
			continue
		}
		if p.ins.ss == ss && p.ins.co == i {
			p.ins = ss.inchain[i]
		} else {
			var lastap reArcp
			for ap = p.ins; ap.ss != nil && !(ap.ss == ss && ap.co == i); ap = ap.ss.inchain[ap.co] {
				lastap = ap
			}
			lastap.ss.inchain[lastap.co] = ss.inchain[i]
		}
		ss.outs[i] = nil
		ss.inchain[i].ss = nil
	}

	if ss.flags&rePOSTSTATE != 0 && ss.lastseen != d.lastpost &&
		(d.lastpost == -1 || d.lastpost < ss.lastseen) {
		d.lastpost = ss.lastseen
	}

	if ss.flags&reNOPROGRESS != 0 && ss.lastseen != d.lastnopr &&
		(d.lastnopr == -1 || d.lastnopr < ss.lastseen) {
		d.lastnopr = ss.lastseen
	}

	return ss
}

// pickss picks the next stateset to be used.
func (v *reExec) pickss(d *reDFA, cp, start int) *reSset {
	if d.nssused < d.nssets {
		i := d.nssused
		d.nssused++
		ss := &d.ssets[i]
		ss.states = d.statesarea[i*d.wordsper : (i+1)*d.wordsper]
		ss.flags = 0
		ss.ins.ss = nil
		ss.ins.co = reWHITE
		ss.outs = d.outsarea[i*d.ncolors : (i+1)*d.ncolors]
		ss.inchain = d.incarea[i*d.ncolors : (i+1)*d.ncolors]
		for j := 0; j < d.ncolors; j++ {
			ss.outs[j] = nil
			ss.inchain[j].ss = nil
		}
		return ss
	}

	var ancient int
	if cp-start > d.nssets*2/3 {
		ancient = cp - d.nssets*2/3
	} else {
		ancient = start
	}
	for i := d.search; i < d.nssets; i++ {
		ss := &d.ssets[i]
		if (ss.lastseen == -1 || ss.lastseen < ancient) && ss.flags&reLOCKED == 0 {
			d.search = i + 1
			return ss
		}
	}
	for i := 0; i < d.search; i++ {
		ss := &d.ssets[i]
		if (ss.lastseen == -1 || ss.lastseen < ancient) && ss.flags&reLOCKED == 0 {
			d.search = i + 1
			return ss
		}
	}

	v.seterr(reASSERT)
	return nil
}
