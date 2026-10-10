package sqlfn

// regexec.c: matching. A chr pointer into the subject is its index, NULL being -1.

// reMatch is regmatch_t: a submatch's start and end, -1 for none.
type reMatch struct{ so, eo int }

// reExec is regexec.c's struct vars.
type reExec struct {
	re        *reRegex
	eflags    int
	nmatch    int
	pmatch    []reMatch
	data      []reChr
	start     int
	searchAt  int
	stop      int
	err       int
	subdfas   []*reDFA
	ladfas    []*reDFA
	lblastcss []*reSset
	lblastcp  []int
	stk       reStack
}

func (v *reExec) iserr() bool { return v.err != 0 }

func (v *reExec) seterr(e int) {
	if v.err == 0 {
		v.err = e
	}
}

// reExecute is pg_regexec: it matches re against data from searchStart, filling pmatch, called
// with stk in use.
func reExecute(re *reRegex, data []reChr, searchStart int, pmatch []reMatch, flags int, stk reStack) int {
	nmatch := len(pmatch)
	if searchStart > len(data) {
		return reNOMATCH
	}
	v := &reExec{re: re, stk: stk}
	v.stk.push(v.stk.fr.regexec)
	if re.info&reUIMPOSSIBLE != 0 {
		return reNOMATCH
	}
	backref := re.info&reUBACKREF != 0
	v.eflags = flags
	if re.cflags&reNOSUB != 0 {
		nmatch = 0
	}
	v.nmatch = nmatch
	if backref {
		v.pmatch = make([]reMatch, re.nsub+1)
		v.nmatch = re.nsub + 1
	} else {
		v.pmatch = pmatch
	}
	if v.nmatch > 0 {
		reZapallsubs(v.pmatch, v.nmatch)
	}
	v.data = data
	v.start = 0
	v.searchAt = searchStart
	v.stop = len(data)
	v.subdfas = make([]*reDFA, re.ntree)
	if n := len(re.lacons); n > 0 {
		v.ladfas = make([]*reDFA, n)
		v.lblastcss = make([]*reSset, n)
		v.lblastcp = make([]int, n)
		for i := range v.lblastcp {
			v.lblastcp[i] = -1
		}
	}

	var st int
	if backref {
		st = v.cfind(&re.tree.cnfa, &re.cmap)
	} else {
		st = v.find(&re.tree.cnfa, &re.cmap)
	}

	if st == reOKAY && backref && nmatch > 0 {
		reZapallsubs(pmatch, nmatch)
		n := nmatch
		if v.nmatch < n {
			n = v.nmatch
		}
		copy(pmatch[:n], v.pmatch[:n])
	}
	return st
}

// getsubdfa creates or re-fetches the DFA for a tree subre node.
func (v *reExec) getsubdfa(t *reSubre) *reDFA {
	d := v.subdfas[t.id]
	if d == nil {
		d = v.newdfa(&t.cnfa, &v.re.cmap)
		if t.op == 'b' {
			d.backno = t.backno
			d.backmin = t.min
			d.backmax = t.max
		}
		v.subdfas[t.id] = d
	}
	return d
}

// getladfa creates or re-fetches the DFA for a LACON subre node.
func (v *reExec) getladfa(n int) *reDFA {
	if v.ladfas[n] == nil {
		sub := &v.re.lacons[n]
		v.ladfas[n] = v.newdfa(&sub.cnfa, &v.re.cmap)
	}
	return v.ladfas[n]
}

// find finds a match for the main NFA (no-complications case).
func (v *reExec) find(cnfa *reCnfa, cm *reColormap) int {
	shorter := v.re.tree.flags&reSHORTER != 0

	s := v.newdfa(&v.re.search, cm)
	cold := -1
	closeAt := v.shortest(s, v.searchAt, v.searchAt, v.stop, &cold, nil)
	if v.iserr() {
		return v.err
	}
	if closeAt == -1 {
		return reNOMATCH
	}
	if v.nmatch == 0 {
		return reOKAY
	}

	open := cold
	cold = -1
	d := v.newdfa(cnfa, cm)
	var begin, end int
	end = -1
	var hitend bool
	for begin = open; begin <= closeAt; begin++ {
		if shorter {
			end = v.shortest(d, begin, begin, v.stop, nil, &hitend)
		} else {
			end = v.longest(d, begin, v.stop, &hitend)
		}
		if v.iserr() {
			return v.err
		}
		if hitend && cold == -1 {
			cold = begin
		}
		if end != -1 {
			break
		}
	}

	v.pmatch[0].so = begin
	v.pmatch[0].eo = end
	if v.nmatch == 1 {
		return reOKAY
	}
	return v.cdissect(v.re.tree, begin, end)
}

// cfind finds a match for the main NFA (with complications).
func (v *reExec) cfind(cnfa *reCnfa, cm *reColormap) int {
	s := v.newdfa(&v.re.search, cm)
	d := v.newdfa(cnfa, cm)
	var cold int
	ret := v.cfindloop(cnfa, cm, d, s, &cold)
	if v.iserr() {
		return v.err
	}
	return ret
}

// cfindloop is the heart of cfind.
func (v *reExec) cfindloop(cnfa *reCnfa, cm *reColormap, d, s *reDFA, coldp *int) int {
	shorter := v.re.tree.flags&reSHORTER != 0
	var hitend bool
	cold := -1
	closeAt := v.searchAt
	for {
		closeAt = v.shortest(s, closeAt, closeAt, v.stop, &cold, nil)
		if v.iserr() {
			*coldp = cold
			return v.err
		}
		if closeAt == -1 {
			break
		}
		open := cold
		cold = -1
		for begin := open; begin <= closeAt; begin++ {
			estart := begin
			estop := v.stop
			for {
				var end int
				if shorter {
					end = v.shortest(d, begin, estart, estop, nil, &hitend)
				} else {
					end = v.longest(d, begin, estop, &hitend)
				}
				if v.iserr() {
					*coldp = cold
					return v.err
				}
				if hitend && cold == -1 {
					cold = begin
				}
				if end == -1 {
					break
				}
				er := v.cdissect(v.re.tree, begin, end)
				if er == reOKAY {
					if v.nmatch > 0 {
						v.pmatch[0].so = begin
						v.pmatch[0].eo = end
					}
					*coldp = cold
					return reOKAY
				}
				if er != reNOMATCH {
					v.seterr(er)
					*coldp = cold
					return er
				}
				if shorter {
					if end == estop {
						break
					}
					estart = end + 1
				} else {
					if end == begin {
						break
					}
					estop = end - 1
				}
			}
		}
		closeAt++
		if !(closeAt < v.stop) {
			break
		}
	}
	*coldp = cold
	return reNOMATCH
}

// reZapallsubs initializes all subexpression matches to "no match", p[0] aside.
func reZapallsubs(p []reMatch, n int) {
	for i := n - 1; i > 0; i-- {
		p[i] = reMatch{-1, -1}
	}
}

// zaptreesubs initializes the subexpressions within a subtree to "no match".
func (v *reExec) zaptreesubs(t *reSubre) {
	if n := t.capno; n > 0 && n < v.nmatch {
		v.pmatch[n] = reMatch{-1, -1}
	}
	for t2 := t.child; t2 != nil; t2 = t2.sibling {
		v.zaptreesubs(t2)
	}
}

// subset sets the subexpression match data for a successful subre.
func (v *reExec) subset(sub *reSubre, begin, end int) {
	n := sub.capno
	if n >= v.nmatch {
		return
	}
	v.pmatch[n] = reMatch{begin, end}
}

// cdissect checks backrefs and determines subexpression matches.
func (v *reExec) cdissect(t *reSubre, begin, end int) int {
	v.stk.push(v.stk.fr.cdissect)
	defer v.stk.pop(v.stk.fr.cdissect)
	if v.stk.tooDeep() {
		return reETOOBIG
	}
	var er int
	switch t.op {
	case '=':
		er = reOKAY
	case 'b':
		er = v.cbrdissect(t, begin, end)
	case '.':
		if t.child.flags&reSHORTER != 0 {
			er = v.crevcondissect(t, begin, end)
		} else {
			er = v.ccondissect(t, begin, end)
		}
	case '|':
		er = v.caltdissect(t, begin, end)
	case '*':
		if t.child.flags&reSHORTER != 0 {
			er = v.creviterdissect(t, begin, end)
		} else {
			er = v.citerdissect(t, begin, end)
		}
	case '(':
		er = v.cdissect(t.child, begin, end)
	default:
		er = reASSERT
	}
	if t.capno > 0 && er == reOKAY {
		v.subset(t, begin, end)
	}
	return er
}

// ccondissect dissects a match for a concatenation node.
func (v *reExec) ccondissect(t *reSubre, begin, end int) int {
	left := t.child
	right := left.sibling

	d := v.getsubdfa(left)
	if v.iserr() {
		return v.err
	}
	d2 := v.getsubdfa(right)
	if v.iserr() {
		return v.err
	}

	mid := v.longest(d, begin, end, nil)
	if v.iserr() {
		return v.err
	}
	if mid == -1 {
		return reNOMATCH
	}

	for {
		if v.longest(d2, mid, end, nil) == end {
			er := v.cdissect(left, begin, mid)
			if er == reOKAY {
				er = v.cdissect(right, mid, end)
				if er == reOKAY {
					return reOKAY
				}
				v.zaptreesubs(left)
			}
			if er != reNOMATCH {
				return er
			}
		}
		if v.iserr() {
			return v.err
		}

		if mid == begin {
			return reNOMATCH
		}
		mid = v.longest(d, begin, mid-1, nil)
		if v.iserr() {
			return v.err
		}
		if mid == -1 {
			return reNOMATCH
		}
	}
}

// crevcondissect dissects a match for a concatenation node, shortest-first.
func (v *reExec) crevcondissect(t *reSubre, begin, end int) int {
	left := t.child
	right := left.sibling

	d := v.getsubdfa(left)
	if v.iserr() {
		return v.err
	}
	d2 := v.getsubdfa(right)
	if v.iserr() {
		return v.err
	}

	mid := v.shortest(d, begin, begin, end, nil, nil)
	if v.iserr() {
		return v.err
	}
	if mid == -1 {
		return reNOMATCH
	}

	for {
		if v.longest(d2, mid, end, nil) == end {
			er := v.cdissect(left, begin, mid)
			if er == reOKAY {
				er = v.cdissect(right, mid, end)
				if er == reOKAY {
					return reOKAY
				}
				v.zaptreesubs(left)
			}
			if er != reNOMATCH {
				return er
			}
		}
		if v.iserr() {
			return v.err
		}

		if mid == end {
			return reNOMATCH
		}
		mid = v.shortest(d, begin, mid+1, end, nil, nil)
		if v.iserr() {
			return v.err
		}
		if mid == -1 {
			return reNOMATCH
		}
	}
}

// cbrdissect dissects a match for a backref node.
func (v *reExec) cbrdissect(t *reSubre, begin, end int) int {
	n := t.backno
	min := t.min
	max := t.max

	if v.pmatch[n].so == -1 {
		return reNOMATCH
	}
	brstring := v.pmatch[n].so
	brlen := v.pmatch[n].eo - v.pmatch[n].so

	if brlen == 0 {
		if begin == end && min <= max {
			return reOKAY
		}
		return reNOMATCH
	}
	if begin == end {
		if min == 0 {
			return reOKAY
		}
		return reNOMATCH
	}

	tlen := end - begin
	if tlen%brlen != 0 {
		return reNOMATCH
	}
	numreps := tlen / brlen
	if numreps < min || (numreps > max && max != reDUPINF) {
		return reNOMATCH
	}

	p := begin
	for ; numreps > 0; numreps-- {
		if reCmp(v.re.icase, v.data[brstring:brstring+brlen], v.data[p:p+brlen]) {
			return reNOMATCH
		}
		p += brlen
	}
	return reOKAY
}

// caltdissect dissects a match for an alternation node.
func (v *reExec) caltdissect(t *reSubre, begin, end int) int {
	for t = t.child; t != nil; t = t.sibling {
		d := v.getsubdfa(t)
		if v.iserr() {
			return v.err
		}
		if v.longest(d, begin, end, nil) == end {
			er := v.cdissect(t, begin, end)
			if er != reNOMATCH {
				return er
			}
		}
		if v.iserr() {
			return v.err
		}
	}
	return reNOMATCH
}

// citerdissect dissects a match for an iteration node.
func (v *reExec) citerdissect(t *reSubre, begin, end int) int {
	minMatches := t.min
	if minMatches <= 0 {
		minMatches = 1
	}

	maxMatches := end - begin
	if maxMatches > t.max && t.max != reDUPINF {
		maxMatches = t.max
	}
	if maxMatches < minMatches {
		maxMatches = minMatches
	}
	endpts := make([]int, maxMatches+1)
	endpts[0] = begin

	d := v.getsubdfa(t.child)
	if v.iserr() {
		return v.err
	}

	nverified := 0
	k := 1
	limit := end

	for k > 0 {
		endpts[k] = v.longest(d, endpts[k-1], limit, nil)
		if v.iserr() {
			return v.err
		}
		if endpts[k] == -1 {
			k--
			goto backtrack
		}

		if nverified >= k {
			nverified = k - 1
		}

		if endpts[k] != end {
			if k >= maxMatches {
				k--
				goto backtrack
			}
			if endpts[k] == endpts[k-1] &&
				(k >= minMatches || minMatches-k < end-endpts[k]) {
				goto backtrack
			}
			k++
			limit = end
			continue
		}

		if k < minMatches {
			goto backtrack
		}

		{
			i := nverified + 1
			for ; i <= k; i++ {
				v.zaptreesubs(t.child)
				er := v.cdissect(t.child, endpts[i-1], endpts[i])
				if er == reOKAY {
					nverified = i
					continue
				}
				if er == reNOMATCH {
					break
				}
				return er
			}
			if i > k {
				return reOKAY
			}
			k = i
		}

	backtrack:
		for k > 0 {
			prevEnd := endpts[k-1]
			if endpts[k] > prevEnd {
				limit = endpts[k] - 1
				if limit > prevEnd || (k < minMatches && minMatches-k >= end-prevEnd) {
					break
				}
			}
			k--
		}
	}

	if t.min == 0 && begin == end {
		return reOKAY
	}
	return reNOMATCH
}

// creviterdissect dissects a match for an iteration node, shortest-first.
func (v *reExec) creviterdissect(t *reSubre, begin, end int) int {
	minMatches := t.min
	if minMatches <= 0 {
		if begin == end {
			return reOKAY
		}
		minMatches = 1
	}

	maxMatches := end - begin
	if maxMatches > t.max && t.max != reDUPINF {
		maxMatches = t.max
	}
	if maxMatches < minMatches {
		maxMatches = minMatches
	}
	endpts := make([]int, maxMatches+1)
	endpts[0] = begin

	d := v.getsubdfa(t.child)
	if v.iserr() {
		return v.err
	}

	nverified := 0
	k := 1
	limit := begin

	for k > 0 {
		if limit == endpts[k-1] && limit != end &&
			(k >= minMatches || minMatches-k < end-limit) {
			limit++
		}

		if k >= maxMatches {
			limit = end
		}

		endpts[k] = v.shortest(d, endpts[k-1], limit, end, nil, nil)
		if v.iserr() {
			return v.err
		}
		if endpts[k] == -1 {
			k--
			goto backtrack
		}

		if nverified >= k {
			nverified = k - 1
		}

		if endpts[k] != end {
			if k >= maxMatches {
				k--
				goto backtrack
			}
			k++
			limit = endpts[k-1]
			continue
		}

		if k < minMatches {
			goto backtrack
		}

		{
			i := nverified + 1
			for ; i <= k; i++ {
				v.zaptreesubs(t.child)
				er := v.cdissect(t.child, endpts[i-1], endpts[i])
				if er == reOKAY {
					nverified = i
					continue
				}
				if er == reNOMATCH {
					break
				}
				return er
			}
			if i > k {
				return reOKAY
			}
			k = i
		}

	backtrack:
		for k > 0 {
			if endpts[k] < end {
				limit = endpts[k] + 1
				break
			}
			k--
		}
	}
	return reNOMATCH
}
