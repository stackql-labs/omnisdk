package sqlfn

import "sort"

// regc_nfa.c: NFA utilities.

func (nfa *reNFA) iserr() bool { return nfa.v.iserr() }

func (nfa *reNFA) seterr(e int) { nfa.v.seterr(e) }

// newnfa sets up an NFA.
func (v *reVars) newnfa(cm *reColormap, parent *reNFA) *reNFA {
	nfa := &reNFA{cm: cm, v: v, parent: parent, minmatchall: -1, maxmatchall: -1}
	nfa.bos = [2]reColor{reCOLORLESS, reCOLORLESS}
	nfa.eos = [2]reColor{reCOLORLESS, reCOLORLESS}

	nfa.post = nfa.newfstate('@')
	nfa.pre = nfa.newfstate('>')
	nfa.init = nfa.newstate()
	nfa.final = nfa.newstate()
	if v.iserr() {
		nfa.freenfa()
		return nil
	}
	nfa.rainbow(nfa.cm, rePLAIN, reCOLORLESS, nfa.pre, nfa.init)
	nfa.newarc('^', 1, nfa.pre, nfa.init)
	nfa.newarc('^', 0, nfa.pre, nfa.init)
	nfa.rainbow(nfa.cm, rePLAIN, reCOLORLESS, nfa.final, nfa.post)
	nfa.newarc('$', 1, nfa.final, nfa.post)
	nfa.newarc('$', 0, nfa.final, nfa.post)

	if v.iserr() {
		nfa.freenfa()
		return nil
	}
	return nfa
}

// freenfa frees an entire NFA, returning its space to the compile budget.
func (nfa *reNFA) freenfa() {
	for _, n := range nfa.sbsizes {
		nfa.v.spaceused -= n*reSizeofState + reSizeofBatchHead
	}
	nfa.sbsizes = nil
	for _, n := range nfa.absizes {
		nfa.v.spaceused -= n*reSizeofArc + reSizeofBatchHead
	}
	nfa.absizes = nil
	nfa.nstates = -1
}

// newstate allocates an NFA state, with zero flag value.
func (nfa *reNFA) newstate() *reState {
	var s *reState
	if nfa.freestates != nil {
		s = nfa.freestates
		nfa.freestates = s.next
	} else if len(nfa.sbsizes) > 0 && nfa.lastsbused < nfa.sbsizes[len(nfa.sbsizes)-1] {
		nfa.lastsbused++
		s = &reState{}
	} else {
		if nfa.v.spaceused >= reMAX_COMPILE_SPACE {
			nfa.seterr(reETOOBIG)
			return nil
		}
		nstates := reFIRSTSBSIZE
		if len(nfa.sbsizes) > 0 {
			nstates = nfa.sbsizes[len(nfa.sbsizes)-1] * 2
		}
		if nstates > reMAXSBSIZE {
			nstates = reMAXSBSIZE
		}
		nfa.v.spaceused += nstates*reSizeofState + reSizeofBatchHead
		nfa.sbsizes = append(nfa.sbsizes, nstates)
		nfa.lastsbused = 1
		s = &reState{}
	}

	s.no = nfa.nstates
	nfa.nstates++
	s.flag = 0
	if nfa.states == nil {
		nfa.states = s
	}
	s.nins = 0
	s.ins = nil
	s.nouts = 0
	s.outs = nil
	s.tmp = nil
	s.next = nil
	if nfa.slast != nil {
		nfa.slast.next = s
	}
	s.prev = nfa.slast
	nfa.slast = s
	return s
}

// newfstate allocates an NFA state with a specified flag value.
func (nfa *reNFA) newfstate(flag byte) *reState {
	s := nfa.newstate()
	if s != nil {
		s.flag = flag
	}
	return s
}

// dropstate deletes a state's inarcs and outarcs and frees it.
func (nfa *reNFA) dropstate(s *reState) {
	for s.ins != nil {
		nfa.freearc(s.ins)
	}
	for s.outs != nil {
		nfa.freearc(s.outs)
	}
	nfa.freestate(s)
}

// freestate frees a state, which has no in-arcs or out-arcs.
func (nfa *reNFA) freestate(s *reState) {
	s.no = reFREESTATE
	s.flag = 0
	if s.next != nil {
		s.next.prev = s.prev
	} else {
		nfa.slast = s.prev
	}
	if s.prev != nil {
		s.prev.next = s.next
	} else {
		nfa.states = s.next
	}
	s.prev = nil
	s.next = nfa.freestates
	nfa.freestates = s
}

// newarc sets up a new arc within an NFA, unless it would duplicate one.
func (nfa *reNFA) newarc(t int, co reColor, from, to *reState) {
	if from.nouts <= to.nins {
		for a := from.outs; a != nil; a = a.outchain {
			if a.to == to && a.co == co && a.typ == t {
				return
			}
		}
	} else {
		for a := to.ins; a != nil; a = a.inchain {
			if a.from == from && a.co == co && a.typ == t {
				return
			}
		}
	}
	nfa.createarc(t, co, from, to)
}

// createarc creates a new arc within an NFA, known not to duplicate one.
func (nfa *reNFA) createarc(t int, co reColor, from, to *reState) {
	a := nfa.allocarc()
	if nfa.iserr() {
		return
	}
	a.typ = t
	a.co = co
	a.to = to
	a.from = from

	a.inchain = to.ins
	a.inchainRev = nil
	if to.ins != nil {
		to.ins.inchainRev = a
	}
	to.ins = a
	a.outchain = from.outs
	a.outchainRev = nil
	if from.outs != nil {
		from.outs.outchainRev = a
	}
	from.outs = a

	from.nouts++
	to.nins++

	if reCOLORED(a) && nfa.parent == nil {
		nfa.cm.colorchain(a)
	}
}

// allocarc allocates a new arc within an NFA.
func (nfa *reNFA) allocarc() *reArc {
	if nfa.freearcs != nil {
		a := nfa.freearcs
		nfa.freearcs = a.outchain
		return a
	}
	if len(nfa.absizes) > 0 && nfa.lastabused < nfa.absizes[len(nfa.absizes)-1] {
		nfa.lastabused++
		return &reArc{}
	}
	if nfa.v.spaceused >= reMAX_COMPILE_SPACE {
		nfa.seterr(reETOOBIG)
		return nil
	}
	narcs := reFIRSTABSIZE
	if len(nfa.absizes) > 0 {
		narcs = nfa.absizes[len(nfa.absizes)-1] * 2
	}
	if narcs > reMAXABSIZE {
		narcs = reMAXABSIZE
	}
	nfa.v.spaceused += narcs*reSizeofArc + reSizeofBatchHead
	nfa.absizes = append(nfa.absizes, narcs)
	nfa.lastabused = 1
	return &reArc{}
}

// freearc frees an arc.
func (nfa *reNFA) freearc(victim *reArc) {
	from := victim.from
	to := victim.to

	if reCOLORED(victim) && nfa.parent == nil {
		nfa.cm.uncolorchain(victim)
	}

	predecessor := victim.outchainRev
	if predecessor == nil {
		from.outs = victim.outchain
	} else {
		predecessor.outchain = victim.outchain
	}
	if victim.outchain != nil {
		victim.outchain.outchainRev = predecessor
	}
	from.nouts--

	predecessor = victim.inchainRev
	if predecessor == nil {
		to.ins = victim.inchain
	} else {
		predecessor.inchain = victim.inchain
	}
	if victim.inchain != nil {
		victim.inchain.inchainRev = predecessor
	}
	to.nins--

	victim.typ = 0
	victim.from = nil
	victim.to = nil
	victim.inchain = nil
	victim.inchainRev = nil
	victim.outchain = nil
	victim.outchainRev = nil
	victim.outchain = nfa.freearcs
	nfa.freearcs = victim
}

// changearcsource flips an arc to have a different from state.
func reChangearcsource(a *reArc, newfrom *reState) {
	oldfrom := a.from
	predecessor := a.outchainRev
	if predecessor == nil {
		oldfrom.outs = a.outchain
	} else {
		predecessor.outchain = a.outchain
	}
	if a.outchain != nil {
		a.outchain.outchainRev = predecessor
	}
	oldfrom.nouts--

	a.from = newfrom

	a.outchain = newfrom.outs
	a.outchainRev = nil
	if newfrom.outs != nil {
		newfrom.outs.outchainRev = a
	}
	newfrom.outs = a
	newfrom.nouts++
}

// reChangearctarget flips an arc to have a different to state.
func reChangearctarget(a *reArc, newto *reState) {
	oldto := a.to
	predecessor := a.inchainRev
	if predecessor == nil {
		oldto.ins = a.inchain
	} else {
		predecessor.inchain = a.inchain
	}
	if a.inchain != nil {
		a.inchain.inchainRev = predecessor
	}
	oldto.nins--

	a.to = newto

	a.inchain = newto.ins
	a.inchainRev = nil
	if newto.ins != nil {
		newto.ins.inchainRev = a
	}
	newto.ins = a
	newto.nins++
}

// reHasnonemptyout reports whether a state has a non-EMPTY out arc.
func reHasnonemptyout(s *reState) bool {
	for a := s.outs; a != nil; a = a.outchain {
		if a.typ != reEMPTY {
			return true
		}
	}
	return false
}

// reFindarc finds an arc, if any, from a state with a given type and color.
func reFindarc(s *reState, typ int, co reColor) *reArc {
	for a := s.outs; a != nil; a = a.outchain {
		if a.typ == typ && a.co == co {
			return a
		}
	}
	return nil
}

// cparc allocates a new arc within an NFA, copying details from an old one.
func (nfa *reNFA) cparc(oa *reArc, from, to *reState) {
	nfa.newarc(oa.typ, oa.co, from, to)
}

// sortins sorts the in arcs of a state by from/color/type.
func (nfa *reNFA) sortins(s *reState) {
	n := s.nins
	if n <= 1 {
		return
	}
	sortarray := make([]*reArc, 0, n)
	for a := s.ins; a != nil; a = a.inchain {
		sortarray = append(sortarray, a)
	}
	sort.Slice(sortarray, func(i, j int) bool { return reSortinsCmp(sortarray[i], sortarray[j]) < 0 })
	a := sortarray[0]
	s.ins = a
	a.inchain = sortarray[1]
	a.inchainRev = nil
	i := 1
	for ; i < n-1; i++ {
		a = sortarray[i]
		a.inchain = sortarray[i+1]
		a.inchainRev = sortarray[i-1]
	}
	a = sortarray[i]
	a.inchain = nil
	a.inchainRev = sortarray[i-1]
}

func reSortinsCmp(aa, bb *reArc) int {
	switch {
	case aa.from.no < bb.from.no:
		return -1
	case aa.from.no > bb.from.no:
		return 1
	case aa.co < bb.co:
		return -1
	case aa.co > bb.co:
		return 1
	case aa.typ < bb.typ:
		return -1
	case aa.typ > bb.typ:
		return 1
	}
	return 0
}

// sortouts sorts the out arcs of a state by to/color/type.
func (nfa *reNFA) sortouts(s *reState) {
	n := s.nouts
	if n <= 1 {
		return
	}
	sortarray := make([]*reArc, 0, n)
	for a := s.outs; a != nil; a = a.outchain {
		sortarray = append(sortarray, a)
	}
	sort.Slice(sortarray, func(i, j int) bool { return reSortoutsCmp(sortarray[i], sortarray[j]) < 0 })
	a := sortarray[0]
	s.outs = a
	a.outchain = sortarray[1]
	a.outchainRev = nil
	i := 1
	for ; i < n-1; i++ {
		a = sortarray[i]
		a.outchain = sortarray[i+1]
		a.outchainRev = sortarray[i-1]
	}
	a = sortarray[i]
	a.outchain = nil
	a.outchainRev = sortarray[i-1]
}

func reSortoutsCmp(aa, bb *reArc) int {
	switch {
	case aa.to.no < bb.to.no:
		return -1
	case aa.to.no > bb.to.no:
		return 1
	case aa.co < bb.co:
		return -1
	case aa.co > bb.co:
		return 1
	case aa.typ < bb.typ:
		return -1
	case aa.typ > bb.typ:
		return 1
	}
	return 0
}

// reBulkArcOpUseSort is BULK_ARC_OP_USE_SORT.
func reBulkArcOpUseSort(nsrcarcs, ndestarcs int) bool {
	if nsrcarcs < 4 {
		return false
	}
	return nsrcarcs > 32 || ndestarcs > 32
}

// moveins moves all in arcs of a state to another state.
func (nfa *reNFA) moveins(oldState, newState *reState) {
	if !reBulkArcOpUseSort(oldState.nins, newState.nins) {
		for oldState.ins != nil {
			a := oldState.ins
			nfa.cparc(a, a.from, newState)
			nfa.freearc(a)
		}
		return
	}
	nfa.sortins(oldState)
	nfa.sortins(newState)
	if nfa.iserr() {
		return
	}
	oa := oldState.ins
	na := newState.ins
	for oa != nil && na != nil {
		a := oa
		switch reSortinsCmp(oa, na) {
		case -1:
			oa = oa.inchain
			reChangearctarget(a, newState)
		case 0:
			oa = oa.inchain
			na = na.inchain
			nfa.freearc(a)
		case 1:
			na = na.inchain
		}
	}
	for oa != nil {
		a := oa
		oa = oa.inchain
		reChangearctarget(a, newState)
	}
}

// copyins copies in arcs of a state to another state.
func (nfa *reNFA) copyins(oldState, newState *reState) {
	if !reBulkArcOpUseSort(oldState.nins, newState.nins) {
		for a := oldState.ins; a != nil; a = a.inchain {
			nfa.cparc(a, a.from, newState)
		}
		return
	}
	nfa.sortins(oldState)
	nfa.sortins(newState)
	if nfa.iserr() {
		return
	}
	oa := oldState.ins
	na := newState.ins
	for oa != nil && na != nil {
		a := oa
		switch reSortinsCmp(oa, na) {
		case -1:
			oa = oa.inchain
			nfa.createarc(a.typ, a.co, a.from, newState)
		case 0:
			oa = oa.inchain
			na = na.inchain
		case 1:
			na = na.inchain
		}
	}
	for oa != nil {
		a := oa
		oa = oa.inchain
		nfa.createarc(a.typ, a.co, a.from, newState)
	}
}

// mergeins merges a list of inarcs, not necessarily unique, into a state.
func (nfa *reNFA) mergeins(s *reState, arcarray []*reArc) {
	if len(arcarray) <= 0 {
		return
	}
	nfa.sortins(s)
	if nfa.iserr() {
		return
	}
	sort.Slice(arcarray, func(i, j int) bool { return reSortinsCmp(arcarray[i], arcarray[j]) < 0 })

	j := 0
	for i := 1; i < len(arcarray); i++ {
		if reSortinsCmp(arcarray[j], arcarray[i]) < 0 {
			j++
			arcarray[j] = arcarray[i]
		}
	}
	arccount := j + 1

	i := 0
	na := s.ins
	for i < arccount && na != nil {
		a := arcarray[i]
		switch reSortinsCmp(a, na) {
		case -1:
			nfa.createarc(a.typ, a.co, a.from, s)
			i++
		case 0:
			i++
			na = na.inchain
		case 1:
			na = na.inchain
		}
	}
	for i < arccount {
		a := arcarray[i]
		nfa.createarc(a.typ, a.co, a.from, s)
		i++
	}
}

// moveouts moves all out arcs of a state to another state.
func (nfa *reNFA) moveouts(oldState, newState *reState) {
	if !reBulkArcOpUseSort(oldState.nouts, newState.nouts) {
		for oldState.outs != nil {
			a := oldState.outs
			nfa.cparc(a, newState, a.to)
			nfa.freearc(a)
		}
		return
	}
	nfa.sortouts(oldState)
	nfa.sortouts(newState)
	if nfa.iserr() {
		return
	}
	oa := oldState.outs
	na := newState.outs
	for oa != nil && na != nil {
		a := oa
		switch reSortoutsCmp(oa, na) {
		case -1:
			oa = oa.outchain
			reChangearcsource(a, newState)
		case 0:
			oa = oa.outchain
			na = na.outchain
			nfa.freearc(a)
		case 1:
			na = na.outchain
		}
	}
	for oa != nil {
		a := oa
		oa = oa.outchain
		reChangearcsource(a, newState)
	}
}

// copyouts copies out arcs of a state to another state.
func (nfa *reNFA) copyouts(oldState, newState *reState) {
	if !reBulkArcOpUseSort(oldState.nouts, newState.nouts) {
		for a := oldState.outs; a != nil; a = a.outchain {
			nfa.cparc(a, newState, a.to)
		}
		return
	}
	nfa.sortouts(oldState)
	nfa.sortouts(newState)
	if nfa.iserr() {
		return
	}
	oa := oldState.outs
	na := newState.outs
	for oa != nil && na != nil {
		a := oa
		switch reSortoutsCmp(oa, na) {
		case -1:
			oa = oa.outchain
			nfa.createarc(a.typ, a.co, newState, a.to)
		case 0:
			oa = oa.outchain
			na = na.outchain
		case 1:
			na = na.outchain
		}
	}
	for oa != nil {
		a := oa
		oa = oa.outchain
		nfa.createarc(a.typ, a.co, newState, a.to)
	}
}

// cloneouts copies out arcs of a state to another state pair, changing their type.
func (nfa *reNFA) cloneouts(old, from, to *reState, typ int) {
	for a := old.outs; a != nil; a = a.outchain {
		nfa.newarc(typ, a.co, from, to)
	}
}

// delsub deletes a sub-NFA.
func (nfa *reNFA) delsub(lp, rp *reState) {
	rp.tmp = rp
	nfa.deltraverse(lp, lp)
	if nfa.iserr() {
		return
	}
	rp.tmp = nil
	lp.tmp = nil
}

// deltraverse is the recursive heart of delsub: it destroys a state's out-arcs.
func (nfa *reNFA) deltraverse(leftend, s *reState) {
	stk := &nfa.v.stk
	stk.push(stk.fr.deltraverse)
	defer stk.pop(stk.fr.deltraverse)
	if stk.tooDeep() {
		nfa.seterr(reETOOBIG)
		return
	}
	if s.nouts == 0 {
		return
	}
	if s.tmp != nil {
		return
	}
	s.tmp = s
	for s.outs != nil {
		a := s.outs
		to := a.to
		nfa.deltraverse(leftend, to)
		if nfa.iserr() {
			return
		}
		nfa.freearc(a)
		if to.nins == 0 && to.tmp == nil {
			nfa.freestate(to)
		}
	}
	s.tmp = nil
}

// dupnfa duplicates a sub-NFA.
func (nfa *reNFA) dupnfa(start, stop, from, to *reState) {
	if start == stop {
		nfa.newarc(reEMPTY, 0, from, to)
		return
	}
	stop.tmp = to
	nfa.duptraverse(start, from)
	stop.tmp = nil
	nfa.cleartraverse(start)
}

// duptraverse is the recursive heart of dupnfa.
func (nfa *reNFA) duptraverse(s, stmp *reState) {
	stk := &nfa.v.stk
	stk.push(stk.fr.duptraverse)
	defer stk.pop(stk.fr.duptraverse)
	if stk.tooDeep() {
		nfa.seterr(reETOOBIG)
		return
	}
	if s.tmp != nil {
		return
	}
	if stmp == nil {
		s.tmp = nfa.newstate()
	} else {
		s.tmp = stmp
	}
	if s.tmp == nil {
		return
	}
	for a := s.outs; a != nil && !nfa.iserr(); a = a.outchain {
		nfa.duptraverse(a.to, nil)
		if nfa.iserr() {
			break
		}
		nfa.cparc(a, s.tmp, a.to.tmp)
	}
}

// removeconstraints removes any constraints in a sub-NFA.
func (nfa *reNFA) removeconstraints(start, stop *reState) {
	if start == stop {
		return
	}
	stop.tmp = stop
	nfa.removetraverse(start)
	stop.tmp = nil
	nfa.cleartraverse(start)
}

// removetraverse is the recursive heart of removeconstraints.
func (nfa *reNFA) removetraverse(s *reState) {
	stk := &nfa.v.stk
	stk.push(stk.fr.removetraverse)
	defer stk.pop(stk.fr.removetraverse)
	if stk.tooDeep() {
		nfa.seterr(reETOOBIG)
		return
	}
	if s.tmp != nil {
		return
	}
	s.tmp = s
	var oa *reArc
	for a := s.outs; a != nil && !nfa.iserr(); a = oa {
		nfa.removetraverse(a.to)
		if nfa.iserr() {
			break
		}
		oa = a.outchain
		switch a.typ {
		case rePLAIN, reEMPTY:
		case reAHEAD, reBEHIND, '^', '$', reLACON:
			nfa.newarc(reEMPTY, 0, s, a.to)
			nfa.freearc(a)
		default:
			nfa.seterr(reASSERT)
		}
	}
}

// cleartraverse is the recursive cleanup for algorithms that leave tmp pointers set.
func (nfa *reNFA) cleartraverse(s *reState) {
	stk := &nfa.v.stk
	stk.push(stk.fr.cleartraverse)
	defer stk.pop(stk.fr.cleartraverse)
	if stk.tooDeep() {
		nfa.seterr(reETOOBIG)
		return
	}
	if s.tmp == nil {
		return
	}
	s.tmp = nil
	for a := s.outs; a != nil; a = a.outchain {
		nfa.cleartraverse(a.to)
	}
}

// reSingleColorTransition reports whether getting from s1 to s2 crosses one PLAIN arc.
func reSingleColorTransition(s1, s2 *reState) *reState {
	if s1.nouts == 1 && s1.outs.typ == reEMPTY {
		s1 = s1.outs.to
	}
	if s2.nins == 1 && s2.ins.typ == reEMPTY {
		s2 = s2.ins.from
	}
	if s1 == s2 {
		return nil
	}
	if s1.outs == nil {
		return nil
	}
	for a := s1.outs; a != nil; a = a.outchain {
		if a.typ != rePLAIN || a.to != s2 {
			return nil
		}
	}
	return s1
}

// specialcolors fills in the special colors for an NFA.
func (nfa *reNFA) specialcolors() {
	if nfa.parent == nil {
		nfa.bos[0] = nfa.cm.pseudocolor()
		nfa.bos[1] = nfa.cm.pseudocolor()
		nfa.eos[0] = nfa.cm.pseudocolor()
		nfa.eos[1] = nfa.cm.pseudocolor()
	} else {
		nfa.bos = nfa.parent.bos
		nfa.eos = nfa.parent.eos
	}
}

// optimize reduces an NFA to a form the executor can handle, returning re_info bits.
func (nfa *reNFA) optimize() int {
	stk := &nfa.v.stk
	stk.push(stk.fr.optimize)
	defer stk.pop(stk.fr.optimize)
	nfa.cleanup()
	nfa.fixempties()
	nfa.fixconstraintloops()
	nfa.pullback()
	nfa.pushfwd()
	nfa.cleanup()
	return nfa.analyze()
}

// pullback pulls back constraints backward to eliminate them.
func (nfa *reNFA) pullback() {
	var nexts *reState
	var nexta *reArc
	for {
		progress := false
		for s := nfa.states; s != nil && !nfa.iserr(); s = nexts {
			nexts = s.next
			var intermediates *reState
			for a := s.outs; a != nil && !nfa.iserr(); a = nexta {
				nexta = a.outchain
				if a.typ == '^' || a.typ == reBEHIND {
					if nfa.pull(a, &intermediates) {
						progress = true
					}
				}
			}
			for intermediates != nil {
				ns := intermediates.tmp
				intermediates.tmp = nil
				intermediates = ns
			}
			if (s.nins == 0 || s.nouts == 0) && s.flag == 0 {
				nfa.dropstate(s)
			}
		}
		if !(progress && !nfa.iserr()) {
			break
		}
	}
	if nfa.iserr() {
		return
	}

	for a := nfa.pre.outs; a != nil; a = nexta {
		nexta = a.outchain
		if a.typ == '^' {
			nfa.newarc(rePLAIN, nfa.bos[a.co], a.from, a.to)
			nfa.freearc(a)
		}
	}
}

// pull pulls a back constraint backward past its source state.
func (nfa *reNFA) pull(con *reArc, intermediates **reState) bool {
	from := con.from
	to := con.to
	var s *reState

	if from.flag != 0 {
		return false
	}
	if from.nins == 0 {
		nfa.freearc(con)
		return true
	}

	if from.nouts > 1 {
		s = nfa.newstate()
		if nfa.iserr() {
			return false
		}
		nfa.copyins(from, s)
		nfa.cparc(con, s, to)
		nfa.freearc(con)
		if nfa.iserr() {
			return false
		}
		from = s
		con = from.outs
	}

	var nexta *reArc
	for a := from.ins; a != nil && !nfa.iserr(); a = nexta {
		nexta = a.inchain
		switch nfa.combine(con, a) {
		case reINCOMPATIBLE:
			nfa.freearc(a)
		case reSATISFIED:
		case reCOMPATIBLE:
			for s = *intermediates; s != nil; s = s.tmp {
				if s.ins.from == a.from && s.outs.to == to {
					break
				}
			}
			if s == nil {
				s = nfa.newstate()
				if nfa.iserr() {
					return false
				}
				s.tmp = *intermediates
				*intermediates = s
			}
			nfa.cparc(con, a.from, s)
			nfa.cparc(a, s, to)
			nfa.freearc(a)
		case reREPLACEARC:
			nfa.newarc(a.typ, con.co, a.from, to)
			nfa.freearc(a)
		}
	}

	nfa.moveins(from, to)
	nfa.freearc(con)
	return true
}

// pushfwd pushes forward constraints forward to eliminate them.
func (nfa *reNFA) pushfwd() {
	var nexts *reState
	var nexta *reArc
	for {
		progress := false
		for s := nfa.states; s != nil && !nfa.iserr(); s = nexts {
			nexts = s.next
			var intermediates *reState
			for a := s.ins; a != nil && !nfa.iserr(); a = nexta {
				nexta = a.inchain
				if a.typ == '$' || a.typ == reAHEAD {
					if nfa.push(a, &intermediates) {
						progress = true
					}
				}
			}
			for intermediates != nil {
				ns := intermediates.tmp
				intermediates.tmp = nil
				intermediates = ns
			}
			if (s.nins == 0 || s.nouts == 0) && s.flag == 0 {
				nfa.dropstate(s)
			}
		}
		if !(progress && !nfa.iserr()) {
			break
		}
	}
	if nfa.iserr() {
		return
	}

	for a := nfa.post.ins; a != nil; a = nexta {
		nexta = a.inchain
		if a.typ == '$' {
			nfa.newarc(rePLAIN, nfa.eos[a.co], a.from, a.to)
			nfa.freearc(a)
		}
	}
}

// push pushes a forward constraint forward past its destination state.
func (nfa *reNFA) push(con *reArc, intermediates **reState) bool {
	from := con.from
	to := con.to
	var s *reState

	if to.flag != 0 {
		return false
	}
	if to.nouts == 0 {
		nfa.freearc(con)
		return true
	}

	if to.nins > 1 {
		s = nfa.newstate()
		if nfa.iserr() {
			return false
		}
		nfa.copyouts(to, s)
		nfa.cparc(con, from, s)
		nfa.freearc(con)
		if nfa.iserr() {
			return false
		}
		to = s
		con = to.ins
	}

	var nexta *reArc
	for a := to.outs; a != nil && !nfa.iserr(); a = nexta {
		nexta = a.outchain
		switch nfa.combine(con, a) {
		case reINCOMPATIBLE:
			nfa.freearc(a)
		case reSATISFIED:
		case reCOMPATIBLE:
			for s = *intermediates; s != nil; s = s.tmp {
				if s.ins.from == from && s.outs.to == a.to {
					break
				}
			}
			if s == nil {
				s = nfa.newstate()
				if nfa.iserr() {
					return false
				}
				s.tmp = *intermediates
				*intermediates = s
			}
			nfa.cparc(con, s, a.to)
			nfa.cparc(a, from, s)
			nfa.freearc(a)
		case reREPLACEARC:
			nfa.newarc(a.typ, con.co, from, a.to)
			nfa.freearc(a)
		}
	}

	nfa.moveouts(to, from)
	nfa.freearc(con)
	return true
}

// Results of combine.
const (
	reINCOMPATIBLE = 1
	reSATISFIED    = 2
	reCOMPATIBLE   = 3
	reREPLACEARC   = 4
)

// combine reports what happens when a constraint lands on an arc.
func (nfa *reNFA) combine(con, a *reArc) int {
	ca := func(ct, at int) int { return ct<<8 | at }
	colorConstraint := func() int {
		if con.co == a.co {
			return reSATISFIED
		}
		if con.co == reRAINBOW {
			if nfa.cm.cd[a.co].flags&rePSEUDO == 0 {
				return reSATISFIED
			}
		} else if a.co == reRAINBOW {
			if nfa.cm.cd[con.co].flags&rePSEUDO != 0 {
				return reINCOMPATIBLE
			}
			return reREPLACEARC
		}
		return reINCOMPATIBLE
	}
	switch ca(con.typ, a.typ) {
	case ca('^', rePLAIN), ca('$', rePLAIN):
		return reINCOMPATIBLE
	case ca(reAHEAD, rePLAIN), ca(reBEHIND, rePLAIN):
		return colorConstraint()
	case ca('^', '^'), ca('$', '$'):
		if con.co == a.co {
			return reSATISFIED
		}
		return reINCOMPATIBLE
	case ca(reAHEAD, reAHEAD), ca(reBEHIND, reBEHIND):
		return colorConstraint()
	case ca('^', reBEHIND), ca(reBEHIND, '^'), ca('$', reAHEAD), ca(reAHEAD, '$'):
		return reINCOMPATIBLE
	case ca('^', '$'), ca('^', reAHEAD), ca(reBEHIND, '$'), ca(reBEHIND, reAHEAD),
		ca('$', '^'), ca('$', reBEHIND), ca(reAHEAD, '^'), ca(reAHEAD, reBEHIND),
		ca('^', reLACON), ca(reBEHIND, reLACON), ca('$', reLACON), ca(reAHEAD, reLACON):
		return reCOMPATIBLE
	}
	return reINCOMPATIBLE
}

// fixempties gets rid of EMPTY arcs.
func (nfa *reNFA) fixempties() {
	var nexts *reState
	var a, nexta *reArc

	for s := nfa.states; s != nil && !nfa.iserr(); s = nexts {
		nexts = s.next
		if s.flag != 0 || s.nouts != 1 {
			continue
		}
		a = s.outs
		if a.typ != reEMPTY {
			continue
		}
		if s != a.to {
			nfa.moveins(s, a.to)
		}
		nfa.dropstate(s)
	}

	for s := nfa.states; s != nil && !nfa.iserr(); s = nexts {
		nexts = s.next
		if s.flag != 0 || s.nins != 1 {
			continue
		}
		a = s.ins
		if a.typ != reEMPTY {
			continue
		}
		if s != a.from {
			nfa.moveouts(s, a.from)
		}
		nfa.dropstate(s)
	}

	if nfa.iserr() {
		return
	}

	inarcsorig := make([]*reArc, nfa.nstates)
	totalinarcs := 0
	for s := nfa.states; s != nil; s = s.next {
		inarcsorig[s.no] = s.ins
		totalinarcs += s.nins
	}

	arcarray := make([]*reArc, 0, totalinarcs)

	for s := nfa.states; s != nil && !nfa.iserr(); s = s.next {
		if s.flag == 0 && !reHasnonemptyout(s) {
			continue
		}

		arcarray = arcarray[:0]
		for s2 := nfa.emptyreachable(s, s, inarcsorig); s2 != s; s2 = nexts {
			for a = inarcsorig[s2.no]; a != nil; a = a.inchain {
				if a.typ != reEMPTY {
					arcarray = append(arcarray, a)
				}
			}
			nexts = s2.tmp
			s2.tmp = nil
		}
		s.tmp = nil

		prevnins := s.nins

		nfa.mergeins(s, arcarray)

		nskip := s.nins - prevnins
		a = s.ins
		for ; nskip > 0; nskip-- {
			a = a.inchain
		}
		inarcsorig[s.no] = a
	}

	if nfa.iserr() {
		return
	}

	for s := nfa.states; s != nil; s = s.next {
		for a = s.outs; a != nil; a = nexta {
			nexta = a.outchain
			if a.typ == reEMPTY {
				nfa.freearc(a)
			}
		}
	}

	for s := nfa.states; s != nil; s = nexts {
		nexts = s.next
		if (s.nins == 0 || s.nouts == 0) && s.flag == 0 {
			nfa.dropstate(s)
		}
	}
}

// emptyreachable finds all states that can reach s by EMPTY arcs, chained through tmp.
func (nfa *reNFA) emptyreachable(s, lastfound *reState, inarcsorig []*reArc) *reState {
	stk := &nfa.v.stk
	stk.push(stk.fr.emptyreachable)
	defer stk.pop(stk.fr.emptyreachable)
	if stk.tooDeep() {
		nfa.seterr(reETOOBIG)
		return lastfound
	}
	s.tmp = lastfound
	lastfound = s
	for a := inarcsorig[s.no]; a != nil; a = a.inchain {
		if a.typ == reEMPTY && a.from.tmp == nil {
			lastfound = nfa.emptyreachable(a.from, lastfound, inarcsorig)
		}
	}
	return lastfound
}

// reIsconstraintarc reports whether an arc is of a constraint type.
func reIsconstraintarc(a *reArc) bool {
	switch a.typ {
	case '^', '$', reBEHIND, reAHEAD, reLACON:
		return true
	}
	return false
}

// reHasconstraintout reports whether a state has a constraint out arc.
func reHasconstraintout(s *reState) bool {
	for a := s.outs; a != nil; a = a.outchain {
		if reIsconstraintarc(a) {
			return true
		}
	}
	return false
}

// fixconstraintloops gets rid of loops containing only constraint arcs.
func (nfa *reNFA) fixconstraintloops() {
	var nexts *reState
	var nexta *reArc

	hasconstraints := false
	for s := nfa.states; s != nil && !nfa.iserr(); s = nexts {
		nexts = s.next
		for a := s.outs; a != nil && !nfa.iserr(); a = nexta {
			nexta = a.outchain
			if reIsconstraintarc(a) {
				if a.to == s {
					nfa.freearc(a)
				} else {
					hasconstraints = true
				}
			}
		}
		if s.nouts == 0 && s.flag == 0 {
			nfa.dropstate(s)
		}
	}

	if nfa.iserr() || !hasconstraints {
		return
	}

restart:
	for s := nfa.states; s != nil && !nfa.iserr(); s = s.next {
		if nfa.findconstraintloop(s) {
			goto restart
		}
	}

	if nfa.iserr() {
		return
	}

	for s := nfa.states; s != nil; s = nexts {
		nexts = s.next
		s.tmp = nil
		if (s.nins == 0 || s.nouts == 0) && s.flag == 0 {
			nfa.dropstate(s)
		}
	}
}

// findconstraintloop recursively finds a loop of constraint arcs and breaks it.
func (nfa *reNFA) findconstraintloop(s *reState) bool {
	stk := &nfa.v.stk
	stk.push(stk.fr.findconstraintloop)
	defer stk.pop(stk.fr.findconstraintloop)
	if stk.tooDeep() {
		nfa.seterr(reETOOBIG)
		return true
	}
	if s.tmp != nil {
		if s.tmp == s {
			return false
		}
		nfa.breakconstraintloop(s)
		return true
	}
	for a := s.outs; a != nil; a = a.outchain {
		if reIsconstraintarc(a) {
			sto := a.to
			s.tmp = sto
			if nfa.findconstraintloop(sto) {
				return true
			}
		}
	}
	s.tmp = s
	return false
}

// breakconstraintloop breaks a loop of constraint arcs.
func (nfa *reNFA) breakconstraintloop(sinitial *reState) {
	var refarc *reArc
	s := sinitial
	for {
		nexts := s.tmp
		if refarc == nil {
			narcs := 0
			for a := s.outs; a != nil; a = a.outchain {
				if a.to == nexts && reIsconstraintarc(a) {
					refarc = a
					narcs++
				}
			}
			if narcs > 1 {
				refarc = nil
			}
		}
		s = nexts
		if s == sinitial {
			break
		}
	}

	var shead, stail *reState
	if refarc != nil {
		shead = refarc.from
		stail = refarc.to
	} else {
		shead = sinitial
		stail = sinitial.tmp
	}

	for s = nfa.states; s != nil; s = s.next {
		s.tmp = nil
	}

	sclone := nfa.newstate()
	if sclone == nil {
		return
	}

	nfa.clonesuccessorstates(stail, sclone, shead, refarc, nil, nil, nfa.nstates)

	if nfa.iserr() {
		return
	}

	if sclone.nouts == 0 {
		nfa.freestate(sclone)
		sclone = nil
	}

	var nexta *reArc
	for a := shead.outs; a != nil; a = nexta {
		nexta = a.outchain
		if a.to == stail && reIsconstraintarc(a) {
			if sclone != nil {
				nfa.cparc(a, shead, sclone)
			}
			nfa.freearc(a)
			if nfa.iserr() {
				break
			}
		}
	}
}

// clonesuccessorstates creates a tree of constraint-arc successor states.
func (nfa *reNFA) clonesuccessorstates(ssource, sclone, spredecessor *reState, refarc *reArc,
	curdonemap, outerdonemap []byte, nstates int) {
	stk := &nfa.v.stk
	stk.push(stk.fr.clonesuccessorstates)
	defer stk.pop(stk.fr.clonesuccessorstates)
	if stk.tooDeep() {
		nfa.seterr(reETOOBIG)
		return
	}
	donemap := curdonemap
	if donemap == nil {
		donemap = make([]byte, nstates)
		if outerdonemap != nil {
			copy(donemap, outerdonemap)
		} else {
			donemap[spredecessor.no] = 1
		}
	}

	donemap[ssource.no] = 1

	for a := ssource.outs; a != nil && !nfa.iserr(); a = a.outchain {
		sto := a.to
		if reIsconstraintarc(a) && reHasconstraintout(sto) {
			if donemap[sto.no] != 0 {
				continue
			}

			var prevclone *reState
			for a2 := sclone.outs; a2 != nil; a2 = a2.outchain {
				if a2.to.tmp == sto {
					prevclone = a2.to
					break
				}
			}

			canmerge := false
			if refarc != nil && a.typ == refarc.typ && a.co == refarc.co {
				canmerge = true
			} else {
				for s := sclone; s.ins != nil; s = s.ins.from {
					if s.nins == 1 && a.typ == s.ins.typ && a.co == s.ins.co {
						canmerge = true
						break
					}
				}
			}

			if canmerge {
				if prevclone != nil {
					nfa.dropstate(prevclone)
				}
				nfa.clonesuccessorstates(sto, sclone, spredecessor, refarc, donemap, outerdonemap, nstates)
			} else if prevclone != nil {
				nfa.cparc(a, sclone, prevclone)
			} else {
				stoclone := nfa.newstate()
				if stoclone == nil {
					break
				}
				stoclone.tmp = sto
				nfa.cparc(a, sclone, stoclone)
			}
		} else {
			nfa.cparc(a, sclone, sto)
		}
	}

	if curdonemap == nil {
		for a := sclone.outs; a != nil && !nfa.iserr(); a = a.outchain {
			stoclone := a.to
			sto := stoclone.tmp
			if sto != nil {
				stoclone.tmp = nil
				nfa.clonesuccessorstates(sto, stoclone, spredecessor, refarc, nil, donemap, nstates)
			}
		}
	}
}

// cleanup cleans up an NFA after optimizations.
func (nfa *reNFA) cleanup() {
	if nfa.iserr() {
		return
	}
	stk := &nfa.v.stk
	stk.push(stk.fr.cleanup)
	defer stk.pop(stk.fr.cleanup)
	nfa.markreachable(nfa.pre, nil, nfa.pre)
	nfa.markcanreach(nfa.post, nfa.pre, nfa.post)
	var nexts *reState
	for s := nfa.states; s != nil && !nfa.iserr(); s = nexts {
		nexts = s.next
		if s.tmp != nfa.post && s.flag == 0 {
			nfa.dropstate(s)
		}
	}
	nfa.cleartraverse(nfa.pre)

	n := 0
	for s := nfa.states; s != nil; s = s.next {
		s.no = n
		n++
	}
	nfa.nstates = n
}

// markreachable recursively marks reachable states.
func (nfa *reNFA) markreachable(s, okay, mark *reState) {
	stk := &nfa.v.stk
	stk.push(stk.fr.markreachable)
	defer stk.pop(stk.fr.markreachable)
	if stk.tooDeep() {
		nfa.seterr(reETOOBIG)
		return
	}
	if s.tmp != okay {
		return
	}
	s.tmp = mark
	for a := s.outs; a != nil; a = a.outchain {
		nfa.markreachable(a.to, okay, mark)
	}
}

// markcanreach recursively marks states which can reach here.
func (nfa *reNFA) markcanreach(s, okay, mark *reState) {
	stk := &nfa.v.stk
	stk.push(stk.fr.markcanreach)
	defer stk.pop(stk.fr.markcanreach)
	if stk.tooDeep() {
		nfa.seterr(reETOOBIG)
		return
	}
	if s.tmp != okay {
		return
	}
	s.tmp = mark
	for a := s.ins; a != nil; a = a.inchain {
		nfa.markcanreach(a.from, okay, mark)
	}
}

// analyze ascertains potentially-useful facts about an optimized NFA.
func (nfa *reNFA) analyze() int {
	if nfa.iserr() {
		return 0
	}
	if nfa.pre.outs == nil {
		return reUIMPOSSIBLE
	}
	nfa.checkmatchall()
	for a := nfa.pre.outs; a != nil; a = a.outchain {
		for aa := a.to.outs; aa != nil; aa = aa.outchain {
			if aa.to == nfa.post {
				return reUEMPTYMATCH
			}
		}
	}
	return 0
}

// checkmatchall reports whether the NFA represents no more than a string length test.
func (nfa *reNFA) checkmatchall() {
	if nfa.nstates > reDUPINF*2 {
		return
	}

	for s := nfa.states; s != nil; s = s.next {
		for a := s.outs; a != nil; a = a.outchain {
			if a.typ != rePLAIN {
				return
			}
			if a.co != reRAINBOW {
				if nfa.cm.cd[a.co].flags&rePSEUDO != 0 {
					if s == nfa.pre && (a.co == nfa.bos[0] || a.co == nfa.bos[1]) {
					} else if a.to == nfa.post && (a.co == nfa.eos[0] || a.co == nfa.eos[1]) {
					} else {
						return
					}
				} else {
					return
				}
			}
		}
	}

	if !reCheckOutColorsMatch(nfa.pre, reRAINBOW, nfa.bos[0]) ||
		!reCheckOutColorsMatch(nfa.pre, reRAINBOW, nfa.bos[1]) ||
		!reCheckInColorsMatch(nfa.post, reRAINBOW, nfa.eos[0]) ||
		!reCheckInColorsMatch(nfa.post, reRAINBOW, nfa.eos[1]) {
		return
	}

	haspaths := make([][]bool, nfa.nstates)

	if nfa.checkmatchallRecurse(nfa.pre, haspaths) {
		haspath := haspaths[nfa.pre.no]
		var minmatch, maxmatch int
		for minmatch = 0; minmatch <= reDUPINF+1; minmatch++ {
			if haspath[minmatch] {
				break
			}
		}
		for maxmatch = minmatch; maxmatch < reDUPINF+1; maxmatch++ {
			if !haspath[maxmatch+1] {
				break
			}
		}
		for morematch := maxmatch + 1; morematch <= reDUPINF+1; morematch++ {
			if haspath[morematch] {
				haspath = nil
				break
			}
		}
		if haspath != nil {
			nfa.minmatchall = minmatch - 1
			nfa.maxmatchall = maxmatch - 1
			nfa.flags |= reMATCHALL
		}
	}
}

// checkmatchallRecurse is checkmatchall_recurse, the recursive search for checkmatchall.
func (nfa *reNFA) checkmatchallRecurse(s *reState, haspaths [][]bool) bool {
	stk := &nfa.v.stk
	stk.push(stk.fr.checkmatchall)
	defer stk.pop(stk.fr.checkmatchall)
	if stk.tooDeep() {
		return false
	}
	result := false
	foundloop := false
	haspath := make([]bool, reDUPINF+2)

	s.tmp = s

	for a := s.outs; a != nil; a = a.outchain {
		if a.co != reRAINBOW {
			continue
		}
		if a.to == nfa.post {
			result = true
			haspath[0] = true
		} else if a.to == s {
			foundloop = true
		} else if a.to.tmp != nil {
			result = false
			break
		} else {
			if haspaths[a.to.no] == nil {
				result = nfa.checkmatchallRecurse(a.to, haspaths)
				if !result {
					break
				}
			} else {
				result = true
			}
			nexthaspath := haspaths[a.to.no]
			if nexthaspath[reDUPINF] != nexthaspath[reDUPINF+1] {
				result = false
				break
			}
			for i := 0; i < reDUPINF; i++ {
				haspath[i+1] = haspath[i+1] || nexthaspath[i]
			}
			haspath[reDUPINF+1] = haspath[reDUPINF+1] || nexthaspath[reDUPINF+1]
		}
	}

	if result && foundloop {
		i := 0
		for i = 0; i <= reDUPINF; i++ {
			if haspath[i] {
				break
			}
		}
		for i++; i <= reDUPINF+1; i++ {
			haspath[i] = true
		}
	}

	haspaths[s.no] = haspath
	s.tmp = nil
	return result
}

// reCheckOutColorsMatch reports whether s's co1 and co2 out-arcs reach the same states.
func reCheckOutColorsMatch(s *reState, co1, co2 reColor) bool {
	result := true
	for a := s.outs; a != nil; a = a.outchain {
		if a.co == co1 {
			a.to.tmp = a.to
		}
	}
	for a := s.outs; a != nil; a = a.outchain {
		if a.co == co2 {
			if a.to.tmp != nil {
				a.to.tmp = nil
			} else {
				result = false
			}
		}
	}
	for a := s.outs; a != nil; a = a.outchain {
		if a.co == co1 {
			if a.to.tmp != nil {
				result = false
				a.to.tmp = nil
			}
		}
	}
	return result
}

// reCheckInColorsMatch reports whether the states reaching s by co1 and co2 arcs are the same.
func reCheckInColorsMatch(s *reState, co1, co2 reColor) bool {
	result := true
	for a := s.ins; a != nil; a = a.inchain {
		if a.co == co1 {
			a.from.tmp = a.from
		}
	}
	for a := s.ins; a != nil; a = a.inchain {
		if a.co == co2 {
			if a.from.tmp != nil {
				a.from.tmp = nil
			} else {
				result = false
			}
		}
	}
	for a := s.ins; a != nil; a = a.inchain {
		if a.co == co1 {
			if a.from.tmp != nil {
				result = false
				a.from.tmp = nil
			}
		}
	}
	return result
}

// compact constructs the compact representation of an NFA.
func (nfa *reNFA) compact(cnfa *reCnfa) {
	nstates := 0
	narcs := 0
	for s := nfa.states; s != nil; s = s.next {
		nstates++
		narcs += s.nouts + 1
	}

	cnfa.stflags = make([]byte, nstates)
	cnfa.states = make([][]reCarc, nstates)
	arcs := make([]reCarc, narcs)
	cnfa.nstates = nstates
	cnfa.pre = nfa.pre.no
	cnfa.post = nfa.post.no
	cnfa.bos = nfa.bos
	cnfa.eos = nfa.eos
	cnfa.ncolors = nfa.cm.maxcolor() + 1
	cnfa.flags = nfa.flags
	cnfa.minmatchall = nfa.minmatchall
	cnfa.maxmatchall = nfa.maxmatchall

	ca := 0
	for s := nfa.states; s != nil; s = s.next {
		cnfa.stflags[s.no] = 0
		first := ca
		for a := s.outs; a != nil; a = a.outchain {
			switch a.typ {
			case rePLAIN:
				arcs[ca] = reCarc{co: a.co, to: a.to.no}
				ca++
			case reLACON:
				arcs[ca] = reCarc{co: cnfa.ncolors + a.co, to: a.to.no}
				ca++
				cnfa.flags |= reHASLACONS
			default:
				nfa.seterr(reASSERT)
				return
			}
		}
		list := arcs[first:ca]
		sort.Slice(list, func(i, j int) bool {
			if list[i].co != list[j].co {
				return list[i].co < list[j].co
			}
			return list[i].to < list[j].to
		})
		arcs[ca] = reCarc{co: reCOLORLESS, to: 0}
		ca++
		cnfa.states[s.no] = arcs[first:ca:ca]
	}

	for a := nfa.pre.outs; a != nil; a = a.outchain {
		cnfa.stflags[a.to.no] = reCNFA_NOPROGRESS
	}
	cnfa.stflags[nfa.pre.no] = reCNFA_NOPROGRESS
}
