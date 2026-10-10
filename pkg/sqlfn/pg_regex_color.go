package sqlfn

// regc_color.c: the color map, which maps chrs to equivalence classes ("colors").

func (cm *reColormap) iserr() bool { return cm.v.iserr() }

func (cm *reColormap) seterr(e int) { cm.v.seterr(e) }

// initcm sets up a new colormap.
func (v *reVars) initcm(cm *reColormap) {
	cm.v = v
	cm.cd = make([]reColordesc, reNINLINECDS)
	cm.max = 0
	cm.free = 0

	cd := &cm.cd[0]
	cd.nschrs = reMAX_SIMPLE_CHR + 1
	cd.nuchrs = 1
	cd.sub = reNOSUBCOLOR
	cd.arcs = nil
	cd.firstchr = 0
	cd.flags = 0

	cm.locolormap = make([]reColor, reMAX_SIMPLE_CHR+1)
	cm.classbits = [reNUM_CCLASSES]int{}
	cm.cmranges = nil
	cm.maxarrayrows = 4
	cm.hiarrayrows = 1
	cm.hiarraycols = 1
	cm.hicolormap = make([]reColor, cm.maxarrayrows)
	cm.hicolormap[0] = reWHITE
}

// getcolorhi is pg_reg_getcolor, the slow case of GETCOLOR.
func (cm *reColormap) getcolorhi(c reChr) reColor {
	rownum := 0
	low, high := 0, len(cm.cmranges)
	for low < high {
		middle := low + (high-low)/2
		cmr := &cm.cmranges[middle]
		if c < cmr.cmin {
			high = middle
		} else if c > cmr.cmax {
			low = middle + 1
		} else {
			rownum = cmr.rownum
			break
		}
	}
	if cm.hiarraycols > 1 {
		colnum := cm.cclassColumnIndex(c)
		return cm.hicolormap[rownum*cm.hiarraycols+colnum]
	}
	return cm.hicolormap[rownum]
}

// maxcolor reports the largest color number in use.
func (cm *reColormap) maxcolor() reColor {
	if cm.iserr() {
		return reCOLORLESS
	}
	return cm.max
}

// newcolor finds a new color, which must be assigned at once.
func (cm *reColormap) newcolor() reColor {
	if cm.iserr() {
		return reCOLORLESS
	}
	var co int
	if cm.free != 0 {
		co = cm.free
		cm.free = cm.cd[co].sub
	} else if cm.max < len(cm.cd)-1 {
		cm.max++
		co = cm.max
	} else {
		if cm.max == reMAX_COLOR {
			cm.seterr(reECOLORS)
			return reCOLORLESS
		}
		n := len(cm.cd) * 2
		if n > reMAX_COLOR+1 {
			n = reMAX_COLOR + 1
		}
		newCd := make([]reColordesc, n)
		copy(newCd, cm.cd)
		cm.cd = newCd
		cm.max++
		co = cm.max
	}
	cd := &cm.cd[co]
	cd.nschrs = 0
	cd.nuchrs = 0
	cd.sub = reNOSUBCOLOR
	cd.arcs = nil
	cd.firstchr = 0
	cd.flags = 0
	return co
}

// freecolor frees a color, which must have no arcs or subcolor.
func (cm *reColormap) freecolor(co reColor) {
	if co == reWHITE {
		return
	}
	cd := &cm.cd[co]
	cd.flags = reFREECOL

	if co == cm.max {
		for cm.max > reWHITE && cm.cd[cm.max].flags&reFREECOL != 0 {
			cm.max--
		}
		for cm.free > cm.max {
			cm.free = cm.cd[cm.free].sub
		}
		if cm.free > 0 {
			pco := cm.free
			nco := cm.cd[pco].sub
			for nco > 0 {
				if nco > cm.max {
					nco = cm.cd[nco].sub
					cm.cd[pco].sub = nco
				} else {
					pco = nco
					nco = cm.cd[pco].sub
				}
			}
		}
	} else {
		cd.sub = cm.free
		cm.free = co
	}
}

// pseudocolor allocates a false color, to be managed by other means.
func (cm *reColormap) pseudocolor() reColor {
	co := cm.newcolor()
	if cm.iserr() {
		return reCOLORLESS
	}
	cd := &cm.cd[co]
	cd.nschrs = 0
	cd.nuchrs = 1
	cd.sub = reNOSUBCOLOR
	cd.arcs = nil
	cd.firstchr = 0
	cd.flags = rePSEUDO
	return co
}

// subcolor allocates a new subcolor (if necessary) to a low chr.
func (cm *reColormap) subcolor(c reChr) reColor {
	co := cm.locolormap[c]
	sco := cm.newsub(co)
	if cm.iserr() {
		return reCOLORLESS
	}
	if co == sco {
		return co
	}
	cm.cd[co].nschrs--
	if cm.cd[sco].nschrs == 0 {
		cm.cd[sco].firstchr = c
	}
	cm.cd[sco].nschrs++
	cm.locolormap[c] = sco
	return sco
}

// subcolorhi allocates a new subcolor (if necessary) to a high colormap entry.
func (cm *reColormap) subcolorhi(pco *reColor) reColor {
	co := *pco
	sco := cm.newsub(co)
	if cm.iserr() {
		return reCOLORLESS
	}
	if co == sco {
		return co
	}
	cm.cd[co].nuchrs--
	cm.cd[sco].nuchrs++
	*pco = sco
	return sco
}

// newsub allocates a new subcolor (if necessary) for a color.
func (cm *reColormap) newsub(co reColor) reColor {
	sco := cm.cd[co].sub
	if sco == reNOSUBCOLOR {
		if cm.cd[co].nschrs+cm.cd[co].nuchrs == 1 {
			return co
		}
		sco = cm.newcolor()
		if sco == reCOLORLESS {
			return reCOLORLESS
		}
		cm.cd[co].sub = sco
		cm.cd[sco].sub = sco
	}
	return sco
}

// newhicolorrow gets a new row in the hicolormap, cloning it from oldrow.
func (cm *reColormap) newhicolorrow(oldrow int) int {
	newrow := cm.hiarrayrows
	if newrow >= cm.maxarrayrows {
		if cm.maxarrayrows >= 0x7fffffff/(cm.hiarraycols*2) {
			cm.seterr(reESPACE)
			return 0
		}
		newarray := make([]reColor, cm.maxarrayrows*2*cm.hiarraycols)
		copy(newarray, cm.hicolormap)
		cm.hicolormap = newarray
		cm.maxarrayrows *= 2
	}
	cm.hiarrayrows++

	cols := cm.hiarraycols
	copy(cm.hicolormap[newrow*cols:newrow*cols+cols], cm.hicolormap[oldrow*cols:oldrow*cols+cols])
	for i := 0; i < cols; i++ {
		cm.cd[cm.hicolormap[newrow*cols+i]].nuchrs++
	}
	return newrow
}

// newhicolorcols extends the high colormap to the right with a copy of itself.
func (cm *reColormap) newhicolorcols() {
	if cm.hiarraycols >= 0x7fffffff/(cm.maxarrayrows*2) {
		cm.seterr(reESPACE)
		return
	}
	cols := cm.hiarraycols
	newarray := make([]reColor, cm.maxarrayrows*cols*2)
	for r := cm.hiarrayrows - 1; r >= 0; r-- {
		for c := 0; c < cols; c++ {
			co := cm.hicolormap[r*cols+c]
			newarray[r*cols*2+c] = co
			newarray[r*cols*2+cols+c] = co
			cm.cd[co].nuchrs++
		}
	}
	cm.hicolormap = newarray
	cm.hiarraycols *= 2
}

// subcolorcvec allocates new subcolors to a cvec's members and fills in arcs.
func (v *reVars) subcolorcvec(cv *reCvec, lp, rp *reState) {
	cm := v.cm
	lastsubcolor := reColor(reCOLORLESS)

	for _, ch := range cv.chrs {
		v.subcoloronechr(ch, lp, rp, &lastsubcolor)
		if v.iserr() {
			return
		}
	}

	for i := 0; i+1 < len(cv.ranges); i += 2 {
		from := cv.ranges[i]
		to := cv.ranges[i+1]
		if from <= reMAX_SIMPLE_CHR {
			lim := to
			if lim > reMAX_SIMPLE_CHR {
				lim = reMAX_SIMPLE_CHR
			}
			for from <= lim {
				sco := cm.subcolor(from)
				if v.iserr() {
					return
				}
				if sco != lastsubcolor {
					v.nfa.newarc(rePLAIN, sco, lp, rp)
					if v.iserr() {
						return
					}
					lastsubcolor = sco
				}
				from++
			}
		}
		if from < to {
			v.subcoloronerange(from, to, lp, rp, &lastsubcolor)
		} else if from == to {
			v.subcoloronechr(from, lp, rp, &lastsubcolor)
		}
		if v.iserr() {
			return
		}
	}

	if cv.cclasscode >= 0 {
		if cm.classbits[cv.cclasscode] == 0 {
			cm.classbits[cv.cclasscode] = cm.hiarraycols
			cm.newhicolorcols()
			if v.iserr() {
				return
			}
		}
		classbit := cm.classbits[cv.cclasscode]
		pco := 0
		for r := 0; r < cm.hiarrayrows; r++ {
			for c := 0; c < cm.hiarraycols; c++ {
				if c&classbit != 0 {
					sco := cm.subcolorhi(&cm.hicolormap[pco])
					if v.iserr() {
						return
					}
					if sco != lastsubcolor {
						v.nfa.newarc(rePLAIN, sco, lp, rp)
						if v.iserr() {
							return
						}
						lastsubcolor = sco
					}
				}
				pco++
			}
		}
	}
}

// subcoloronechr does subcolorcvec's work for a single chr.
func (v *reVars) subcoloronechr(ch reChr, lp, rp *reState, lastsubcolor *reColor) {
	cm := v.cm

	if ch <= reMAX_SIMPLE_CHR {
		sco := cm.subcolor(ch)
		if v.iserr() {
			return
		}
		if sco != *lastsubcolor {
			v.nfa.newarc(rePLAIN, sco, lp, rp)
			*lastsubcolor = sco
		}
		return
	}

	newranges := make([]reCmrange, 0, len(cm.cmranges)+2)
	old := cm.cmranges
	oi := 0
	for ; oi < len(old); oi++ {
		if old[oi].cmax >= ch {
			break
		}
		newranges = append(newranges, old[oi])
	}

	var newrow int
	if oi >= len(old) || old[oi].cmin > ch {
		newrow = cm.newhicolorrow(0)
		newranges = append(newranges, reCmrange{cmin: ch, cmax: ch, rownum: newrow})
	} else if old[oi].cmin == old[oi].cmax {
		newranges = append(newranges, old[oi])
		newrow = old[oi].rownum
		oi++
	} else {
		o := old[oi]
		if ch > o.cmin {
			newranges = append(newranges, reCmrange{cmin: o.cmin, cmax: ch - 1, rownum: o.rownum})
		}
		newrow = cm.newhicolorrow(o.rownum)
		newranges = append(newranges, reCmrange{cmin: ch, cmax: ch, rownum: newrow})
		if ch < o.cmax {
			row := o.rownum
			if ch > o.cmin {
				row = cm.newhicolorrow(o.rownum)
			}
			newranges = append(newranges, reCmrange{cmin: ch + 1, cmax: o.cmax, rownum: row})
		}
		oi++
	}

	v.subcoloronerow(newrow, lp, rp, lastsubcolor)

	newranges = append(newranges, old[oi:]...)
	cm.cmranges = newranges
}

// subcoloronerange does subcolorcvec's work for a high range.
func (v *reVars) subcoloronerange(from, to reChr, lp, rp *reState, lastsubcolor *reColor) {
	cm := v.cm
	newranges := make([]reCmrange, 0, len(cm.cmranges)*2+1)
	old := cm.cmranges
	oi := 0
	for ; oi < len(old); oi++ {
		if old[oi].cmax >= from {
			break
		}
		newranges = append(newranges, old[oi])
	}

	var newrow int
	for oi < len(old) && old[oi].cmin <= to {
		o := old[oi]
		if from < o.cmin {
			newrow = cm.newhicolorrow(0)
			newranges = append(newranges, reCmrange{cmin: from, cmax: o.cmin - 1, rownum: newrow})
			v.subcoloronerow(newrow, lp, rp, lastsubcolor)
			from = o.cmin
		}

		if from <= o.cmin && to >= o.cmax {
			newranges = append(newranges, o)
			newrow = o.rownum
			from = o.cmax + 1
		} else {
			if from > o.cmin {
				newranges = append(newranges, reCmrange{cmin: o.cmin, cmax: from - 1, rownum: o.rownum})
			}
			cmax := o.cmax
			if to < o.cmax {
				cmax = to
			}
			newrow = cm.newhicolorrow(o.rownum)
			newranges = append(newranges, reCmrange{cmin: from, cmax: cmax, rownum: newrow})
			if to < o.cmax {
				row := o.rownum
				if from > o.cmin {
					row = cm.newhicolorrow(o.rownum)
				}
				newranges = append(newranges, reCmrange{cmin: to + 1, cmax: o.cmax, rownum: row})
			}
			from = o.cmax + 1
		}
		v.subcoloronerow(newrow, lp, rp, lastsubcolor)
		oi++
	}

	if from <= to {
		newrow = cm.newhicolorrow(0)
		newranges = append(newranges, reCmrange{cmin: from, cmax: to, rownum: newrow})
		v.subcoloronerow(newrow, lp, rp, lastsubcolor)
	}

	newranges = append(newranges, old[oi:]...)
	cm.cmranges = newranges
}

// subcoloronerow does subcolorcvec's work for one new row in the high colormap.
func (v *reVars) subcoloronerow(rownum int, lp, rp *reState, lastsubcolor *reColor) {
	cm := v.cm
	base := rownum * cm.hiarraycols
	for i := 0; i < cm.hiarraycols; i++ {
		sco := cm.subcolorhi(&cm.hicolormap[base+i])
		if v.iserr() {
			return
		}
		if sco != *lastsubcolor {
			v.nfa.newarc(rePLAIN, sco, lp, rp)
			if v.iserr() {
				return
			}
			*lastsubcolor = sco
		}
	}
}

// okcolors promotes subcolors to full colors.
func (cm *reColormap) okcolors(nfa *reNFA) {
	end := cm.max + 1
	for co := 0; co < end; co++ {
		cd := &cm.cd[co]
		sco := cd.sub
		if cd.flags&reFREECOL != 0 || sco == reNOSUBCOLOR {
			// has no subcolor, no further action
		} else if sco == co {
			// is subcolor, let parent deal with it
		} else if cd.nschrs == 0 && cd.nuchrs == 0 {
			cd.sub = reNOSUBCOLOR
			cm.cd[sco].sub = reNOSUBCOLOR
			for {
				a := cm.cd[co].arcs
				if a == nil {
					break
				}
				cm.uncolorchain(a)
				a.co = sco
				cm.colorchain(a)
			}
			cm.freecolor(co)
		} else {
			cd.sub = reNOSUBCOLOR
			cm.cd[sco].sub = reNOSUBCOLOR
			for a := cm.cd[co].arcs; a != nil; a = a.colorchain {
				nfa.newarc(a.typ, sco, a.from, a.to)
			}
		}
	}
}

// colorchain adds an arc to the color chain of its color.
func (cm *reColormap) colorchain(a *reArc) {
	cd := &cm.cd[a.co]
	if cd.arcs != nil {
		cd.arcs.colorchainRev = a
	}
	a.colorchain = cd.arcs
	a.colorchainRev = nil
	cd.arcs = a
}

// uncolorchain deletes an arc from the color chain of its color.
func (cm *reColormap) uncolorchain(a *reArc) {
	cd := &cm.cd[a.co]
	aa := a.colorchainRev
	if aa == nil {
		cd.arcs = a.colorchain
	} else {
		aa.colorchain = a.colorchain
	}
	if a.colorchain != nil {
		a.colorchain.colorchainRev = aa
	}
	a.colorchain = nil
	a.colorchainRev = nil
}

// rainbow adds arcs of all full colors (but one) between two states.
func (nfa *reNFA) rainbow(cm *reColormap, typ int, but reColor, from, to *reState) {
	if but == reCOLORLESS {
		nfa.newarc(typ, reRAINBOW, from, to)
		return
	}
	end := cm.max + 1
	for co := 0; co < end && !cm.iserr(); co++ {
		cd := &cm.cd[co]
		if cd.flags&reFREECOL == 0 && cd.sub != co && co != but && cd.flags&rePSEUDO == 0 {
			nfa.newarc(typ, co, from, to)
		}
	}
}

// colorcomplement adds arcs of the colors not among of's PLAIN outarcs.
func (nfa *reNFA) colorcomplement(cm *reColormap, typ int, of, from, to *reState) {
	if reFindarc(of, rePLAIN, reRAINBOW) != nil {
		return
	}
	for a := of.outs; a != nil; a = a.outchain {
		if a.typ == rePLAIN {
			cm.cd[a.co].flags |= reCOLMARK
		}
	}
	end := cm.max + 1
	for co := 0; co < end && !cm.iserr(); co++ {
		cd := &cm.cd[co]
		if cd.flags&reCOLMARK != 0 {
			cd.flags &^= reCOLMARK
		} else if cd.flags&reFREECOL == 0 && cd.flags&rePSEUDO == 0 {
			nfa.newarc(typ, co, from, to)
		}
	}
}
