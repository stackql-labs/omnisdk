package sqlfn

import "math"

// musl's libm, ported from the musl snapshot modernc.org/libc transpiles for Linux (7ada6dde): on
// Linux, stackql's embedded SQLite computes its math functions with exactly this code, so this port
// is what makes cos, pow and the rest agree with it to the last bit. Every product is rounded on its
// own, float64(a*b), as C (and ccgo's transpilation of it) does: Go would otherwise be free to fuse
// a*b+c into one rounding on FMA hardware. Where musl itself uses a fused multiply-add — on targets
// it builds with __FP_FAST_FMA — the port calls math.FMA, as fma selects.

type muslLibm struct{ fma bool }

func asuint64(f float64) uint64 { return math.Float64bits(f) }
func asdouble(i uint64) float64 { return math.Float64frombits(i) }
func top12(x float64) uint32    { return uint32(asuint64(x) >> 52) }
func top16(x float64) uint32    { return uint32(asuint64(x) >> 48) }

var zero = 0.0 // a divisor Go cannot fold

func mathOflow(sign uint32) float64 {
	if sign != 0 {
		return math.Inf(-1)
	}
	return math.Inf(1)
}

func mathUflow(sign uint32) float64 {
	if sign != 0 {
		return math.Copysign(0, -1)
	}
	return 0
}

func mathDivzero(sign uint32) float64 {
	if sign != 0 {
		return -1 / zero
	}
	return 1 / zero
}

func mathInvalid(x float64) float64 { return (x - x) / (x - x) }

func highWord(x float64) uint32 { return uint32(asuint64(x) >> 32) }
func lowWord(x float64) uint32  { return uint32(asuint64(x)) }
func withLowWord(x float64, lo uint32) float64 {
	return asdouble(asuint64(x)&0xffffffff00000000 | uint64(lo))
}

// exp

func expSpecialcase(tmp float64, sbits, ki uint64) float64 {
	if ki&0x80000000 == 0 {
		sbits -= 1009 << 52
		scale := asdouble(sbits)
		return float64(0x1p1009 * (scale + float64(scale*tmp)))
	}
	sbits += 1022 << 52
	scale := asdouble(sbits)
	y := scale + float64(scale*tmp)
	if y < 1.0 {
		lo := scale - y + float64(scale*tmp)
		hi := 1.0 + y
		lo = 1.0 - hi + y + lo
		y = hi + lo - 1.0
		if y == 0.0 {
			y = 0.0
		}
	}
	return float64(0x1p-1022 * y)
}

func (m muslLibm) Exp(x float64) float64 {
	abstop := top12(x) & 0x7ff
	if abstop-top12(0x1p-54) >= top12(512.0)-top12(0x1p-54) {
		if abstop-top12(0x1p-54) >= 0x80000000 {
			return 1.0 + x
		}
		if abstop >= top12(1024.0) {
			if asuint64(x) == asuint64(math.Inf(-1)) {
				return 0.0
			}
			if abstop >= top12(math.Inf(1)) {
				return 1.0 + x
			}
			if asuint64(x)>>63 != 0 {
				return mathUflow(0)
			}
			return mathOflow(0)
		}
		abstop = 0
	}
	e := &muslExp
	z := float64(e.invln2N * x)
	kd := z + e.shift
	ki := asuint64(kd)
	kd -= e.shift
	r := x + float64(kd*e.negln2hiN) + float64(kd*e.negln2loN)
	idx := 2 * (ki % muslExpN)
	top := ki << (52 - 7)
	tail := asdouble(e.tab[idx])
	sbits := e.tab[idx+1] + top
	r2 := float64(r * r)
	c2, c3, c4, c5 := e.poly[0], e.poly[1], e.poly[2], e.poly[3]
	tmp := tail + r + float64(r2*(c2+float64(r*c3))) + float64(float64(r2*r2)*(c4+float64(r*c5)))
	if abstop == 0 {
		return expSpecialcase(tmp, sbits, ki)
	}
	scale := asdouble(sbits)
	return scale + float64(scale*tmp)
}

// log

func (m muslLibm) Log(x float64) float64 {
	const off = 0x3fe6000000000000
	ix := asuint64(x)
	top := top16(x)
	lo1 := asuint64(1.0 - 0x1p-4)
	hi1 := asuint64(1.0 + 0x1.09p-4)
	d := &muslLog
	B, A := d.poly1, d.poly
	if ix-lo1 < hi1-lo1 {
		if ix == asuint64(1.0) {
			return 0
		}
		r := x - 1.0
		r2 := float64(r * r)
		r3 := float64(r * r2)
		y := float64(r3 * (B[1] + float64(r*B[2]) + float64(r2*B[3]) +
			float64(r3*(B[4]+float64(r*B[5])+float64(r2*B[6])+
				float64(r3*(B[7]+float64(r*B[8])+float64(r2*B[9])+float64(r3*B[10])))))))
		w := float64(r * 0x1p27)
		rhi := r + w - w
		rlo := r - rhi
		w = float64(float64(rhi*rhi) * B[0])
		hi := r + w
		lo := r - hi + w
		lo += float64(float64(B[0]*rlo) * (rhi + r))
		y += lo
		y += hi
		return y
	}
	if top-0x0010 >= 0x7ff0-0x0010 {
		if ix*2 == 0 {
			return mathDivzero(1)
		}
		if ix == asuint64(math.Inf(1)) {
			return x
		}
		if top&0x8000 != 0 || top&0x7ff0 == 0x7ff0 {
			return mathInvalid(x)
		}
		ix = asuint64(float64(x * 0x1p52))
		ix -= 52 << 52
	}
	tmp := ix - off
	i := (tmp >> (52 - 7)) % 128
	k := int64(tmp) >> 52
	iz := ix - (tmp & (0xfff << 52))
	invc, logc := d.tab[i][0], d.tab[i][1]
	z := asdouble(iz)
	var r float64
	if m.fma {
		r = math.FMA(z, invc, -1.0)
	} else {
		r = float64((z - d.tab2[i][0] - d.tab2[i][1]) * invc)
	}
	kd := float64(k)
	w := float64(kd*d.hi) + logc
	hi := w + r
	lo := w - hi + r + float64(kd*d.lo)
	r2 := float64(r * r)
	y := lo + float64(r2*A[0]) + float64(float64(r*r2)*(A[1]+float64(r*A[2])+float64(r2*(A[3]+float64(r*A[4]))))) + hi
	return y
}

// log2

func (m muslLibm) Log2(x float64) float64 {
	const off = 0x3fe6000000000000
	ix := asuint64(x)
	top := top16(x)
	lo1 := asuint64(1.0 - 0x1.5b51p-5)
	hi1 := asuint64(1.0 + 0x1.6ab2p-5)
	d := &muslLog2
	B, A := d.poly1, d.poly
	if ix-lo1 < hi1-lo1 {
		if ix == asuint64(1.0) {
			return 0
		}
		r := x - 1.0
		var hi, lo float64
		if m.fma {
			hi = float64(r * d.hi)
			lo = float64(r*d.lo) + math.FMA(r, d.hi, -hi)
		} else {
			rhi := asdouble(asuint64(r) & mask32)
			rlo := r - rhi
			hi = float64(rhi * d.hi)
			lo = float64(rlo*d.hi) + float64(r*d.lo)
		}
		r2 := float64(r * r)
		r4 := float64(r2 * r2)
		p := float64(r2 * (B[0] + float64(r*B[1])))
		y := hi + p
		lo += hi - y + p
		lo += float64(r4 * (B[2] + float64(r*B[3]) + float64(r2*(B[4]+float64(r*B[5]))) +
			float64(r4*(B[6]+float64(r*B[7])+float64(r2*(B[8]+float64(r*B[9])))))))
		y += lo
		return y
	}
	if top-0x0010 >= 0x7ff0-0x0010 {
		if ix*2 == 0 {
			return mathDivzero(1)
		}
		if ix == asuint64(math.Inf(1)) {
			return x
		}
		if top&0x8000 != 0 || top&0x7ff0 == 0x7ff0 {
			return mathInvalid(x)
		}
		ix = asuint64(float64(x * 0x1p52))
		ix -= 52 << 52
	}
	tmp := ix - off
	i := (tmp >> (52 - 6)) % 64
	k := int64(tmp) >> 52
	iz := ix - (tmp & (0xfff << 52))
	invc, logc := d.tab[i][0], d.tab[i][1]
	z := asdouble(iz)
	kd := float64(k)
	var r, t1, t2 float64
	if m.fma {
		r = math.FMA(z, invc, -1.0)
		t1 = float64(r * d.hi)
		t2 = float64(r*d.lo) + math.FMA(r, d.hi, -t1)
	} else {
		r = float64((z - d.tab2[i][0] - d.tab2[i][1]) * invc)
		rhi := asdouble(asuint64(r) & mask32)
		rlo := r - rhi
		t1 = float64(rhi * d.hi)
		t2 = float64(rlo*d.hi) + float64(r*d.lo)
	}
	t3 := kd + logc
	hi := t3 + t1
	lo := t3 - hi + t1 + t2
	r2 := float64(r * r)
	r4 := float64(r2 * r2)
	p := A[0] + float64(r*A[1]) + float64(r2*(A[2]+float64(r*A[3]))) + float64(r4*(A[4]+float64(r*A[5])))
	return lo + float64(r2*p) + hi
}

// log10 and log1p (fdlibm)

const (
	lg1 float64 = 6.666666666666735130e-01
	lg2 float64 = 3.999999999940941908e-01
	lg3 float64 = 2.857142874366239149e-01
	lg4 float64 = 2.222219843214978396e-01
	lg5 float64 = 1.818357216161805012e-01
	lg6 float64 = 1.531383769920937332e-01
	lg7 float64 = 1.479819860511658591e-01
)

func (m muslLibm) Log10(x float64) float64 {
	const (
		ivln10hi  float64 = 4.34294481878168880939e-01
		ivln10lo  float64 = 2.50829467116452752298e-11
		log10_2hi float64 = 3.01029995663611771306e-01
		log10_2lo float64 = 3.69423907715893078616e-13
	)
	u := asuint64(x)
	hx := uint32(u >> 32)
	k := 0
	if hx < 0x00100000 || hx>>31 != 0 {
		if u<<1 == 0 {
			return -1 / float64(x*x)
		}
		if hx>>31 != 0 {
			return (x - x) / zero
		}
		k -= 54
		x = float64(x * 0x1p54)
		u = asuint64(x)
		hx = uint32(u >> 32)
	} else if hx >= 0x7ff00000 {
		return x
	} else if hx == 0x3ff00000 && u<<32 == 0 {
		return 0
	}
	hx += 0x3ff00000 - 0x3fe6a09e
	k += int(hx>>20) - 0x3ff
	hx = (hx & 0x000fffff) + 0x3fe6a09e
	u = uint64(hx)<<32 | (u & 0xffffffff)
	x = asdouble(u)
	f := x - 1.0
	hfsq := float64(float64(0.5*f) * f)
	s := f / (2.0 + f)
	z := float64(s * s)
	w := float64(z * z)
	t1 := float64(w * (lg2 + float64(w*(lg4+float64(w*lg6)))))
	t2 := float64(z * (lg1 + float64(w*(lg3+float64(w*(lg5+float64(w*lg7)))))))
	R := t2 + t1
	hi := f - hfsq
	hi = asdouble(asuint64(hi) & mask32)
	lo := f - hi - hfsq + float64(s*(hfsq+R))
	valHi := float64(hi * ivln10hi)
	dk := float64(k)
	y := float64(dk * log10_2hi)
	valLo := float64(dk*log10_2lo) + float64((lo+hi)*ivln10lo) + float64(lo*ivln10hi)
	w = y + valHi
	valLo += (y - w) + valHi
	valHi = w
	return valLo + valHi
}

func muslLog1p(x float64) float64 {
	const (
		ln2Hi float64 = 6.93147180369123816490e-01
		ln2Lo float64 = 1.90821492927058770002e-10
	)
	u := asuint64(x)
	hx := uint32(u >> 32)
	k := 1
	var c, f float64
	if hx < 0x3fda827a || hx>>31 != 0 {
		if hx >= 0xbff00000 {
			if x == -1 {
				return x / zero
			}
			return (x - x) / zero
		}
		if hx<<1 < 0x3ca00000<<1 {
			return x
		}
		if hx <= 0xbfd2bec4 {
			k = 0
			c = 0
			f = x
		}
	} else if hx >= 0x7ff00000 {
		return x
	}
	if k != 0 {
		uf := 1 + x
		u = asuint64(uf)
		hu := uint32(u >> 32)
		hu += 0x3ff00000 - 0x3fe6a09e
		k = int(hu>>20) - 0x3ff
		if k < 54 {
			if k >= 2 {
				c = 1 - (uf - x)
			} else {
				c = x - (uf - 1)
			}
			c /= uf
		} else {
			c = 0
		}
		hu = (hu & 0x000fffff) + 0x3fe6a09e
		u = uint64(hu)<<32 | (u & 0xffffffff)
		f = asdouble(u) - 1
	}
	hfsq := float64(float64(0.5*f) * f)
	s := f / (2.0 + f)
	z := float64(s * s)
	w := float64(z * z)
	t1 := float64(w * (lg2 + float64(w*(lg4+float64(w*lg6)))))
	t2 := float64(z * (lg1 + float64(w*(lg3+float64(w*(lg5+float64(w*lg7)))))))
	R := t2 + t1
	dk := float64(k)
	return float64(s*(hfsq+R)) + (float64(dk*ln2Lo) + c) - hfsq + f + float64(dk*ln2Hi)
}

// pow

func powLogInline(ix uint64, fma bool) (hi, tail float64) {
	const off = 0x3fe6955500000000
	d := &muslPowLog
	A := d.poly
	tmp := ix - off
	i := (tmp >> (52 - 7)) % 128
	k := int64(tmp) >> 52
	iz := ix - (tmp & (0xfff << 52))
	z := asdouble(iz)
	kd := float64(k)
	invc, logc, logctail := d.tab[i][0], d.tab[i][2], d.tab[i][3]
	var r, rhi, rlo float64
	if fma {
		r = math.FMA(z, invc, -1.0)
	} else {
		zhi := asdouble((iz + (1 << 31)) & mask32)
		zlo := z - zhi
		rhi = float64(zhi*invc) - 1.0
		rlo = float64(zlo * invc)
		r = rhi + rlo
	}
	t1 := float64(kd*d.ln2hi) + logc
	t2 := t1 + r
	lo1 := float64(kd*d.ln2lo) + logctail
	lo2 := t1 - t2 + r
	ar := float64(A[0] * r)
	ar2 := float64(r * ar)
	ar3 := float64(r * ar2)
	var lo3, lo4 float64
	if fma {
		hi = t2 + ar2
		lo3 = math.FMA(ar, r, -ar2)
		lo4 = t2 - hi + ar2
	} else {
		arhi := float64(A[0] * rhi)
		arhi2 := float64(rhi * arhi)
		hi = t2 + arhi2
		lo3 = float64(rlo * (ar + arhi))
		lo4 = t2 - hi + arhi2
	}
	p := float64(ar3 * (A[1] + float64(r*A[2]) +
		float64(ar2*(A[3]+float64(r*A[4])+float64(ar2*(A[5]+float64(r*A[6])))))))
	lo := lo1 + lo2 + lo3 + lo4 + p
	y := hi + lo
	tail = hi - y + lo
	return y, tail
}

func powSpecialcase(tmp float64, sbits, ki uint64) float64 {
	if ki&0x80000000 == 0 {
		sbits -= 1009 << 52
		scale := asdouble(sbits)
		return float64(0x1p1009 * (scale + float64(scale*tmp)))
	}
	sbits += 1022 << 52
	scale := asdouble(sbits)
	y := scale + float64(scale*tmp)
	if math.Abs(y) < 1.0 {
		one := 1.0
		if y < 0.0 {
			one = -1.0
		}
		lo := scale - y + float64(scale*tmp)
		hi := one + y
		lo = one - hi + y + lo
		y = hi + lo - one
		if y == 0.0 {
			y = asdouble(sbits & 0x8000000000000000)
		}
	}
	return float64(0x1p-1022 * y)
}

const powSignBias = 0x800 << 7

func powExpInline(x, xtail float64, signBias uint32) float64 {
	abstop := top12(x) & 0x7ff
	if abstop-top12(0x1p-54) >= top12(512.0)-top12(0x1p-54) {
		if abstop-top12(0x1p-54) >= 0x80000000 {
			one := 1.0 + x
			if signBias != 0 {
				return -one
			}
			return one
		}
		if abstop >= top12(1024.0) {
			if asuint64(x)>>63 != 0 {
				return mathUflow(signBias)
			}
			return mathOflow(signBias)
		}
		abstop = 0
	}
	e := &muslExp
	z := float64(e.invln2N * x)
	kd := z + e.shift
	ki := asuint64(kd)
	kd -= e.shift
	r := x + float64(kd*e.negln2hiN) + float64(kd*e.negln2loN)
	r += xtail
	idx := 2 * (ki % muslExpN)
	top := (ki + uint64(signBias)) << (52 - 7)
	tail := asdouble(e.tab[idx])
	sbits := e.tab[idx+1] + top
	r2 := float64(r * r)
	c2, c3, c4, c5 := e.poly[0], e.poly[1], e.poly[2], e.poly[3]
	tmp := tail + r + float64(r2*(c2+float64(r*c3))) + float64(float64(r2*r2)*(c4+float64(r*c5)))
	if abstop == 0 {
		return powSpecialcase(tmp, sbits, ki)
	}
	scale := asdouble(sbits)
	return scale + float64(scale*tmp)
}

func powCheckint(iy uint64) int {
	e := int(iy >> 52 & 0x7ff)
	if e < 0x3ff {
		return 0
	}
	if e > 0x3ff+52 {
		return 2
	}
	if iy&((1<<(0x3ff+52-e))-1) != 0 {
		return 0
	}
	if iy&(1<<(0x3ff+52-e)) != 0 {
		return 1
	}
	return 2
}

func zeroinfnan(i uint64) bool { return 2*i-1 >= 2*asuint64(math.Inf(1))-1 }

func (m muslLibm) Pow(x, y float64) float64 {
	var signBias uint32
	ix := asuint64(x)
	iy := asuint64(y)
	topx := top12(x)
	topy := top12(y)
	inf := asuint64(math.Inf(1))
	if topx-0x001 >= 0x7ff-0x001 || (topy&0x7ff)-0x3be >= 0x43e-0x3be {
		if zeroinfnan(iy) {
			if 2*iy == 0 {
				return 1.0
			}
			if ix == asuint64(1.0) {
				return 1.0
			}
			if 2*ix > 2*inf || 2*iy > 2*inf {
				return x + y
			}
			if 2*ix == 2*asuint64(1.0) {
				return 1.0
			}
			if (2*ix < 2*asuint64(1.0)) == (iy>>63 == 0) {
				return 0.0
			}
			return float64(y * y)
		}
		if zeroinfnan(ix) {
			x2 := float64(x * x)
			if ix>>63 != 0 && powCheckint(iy) == 1 {
				x2 = -x2
			}
			if iy>>63 != 0 {
				return 1 / x2
			}
			return x2
		}
		if ix>>63 != 0 {
			yint := powCheckint(iy)
			if yint == 0 {
				return mathInvalid(x)
			}
			if yint == 1 {
				signBias = powSignBias
			}
			ix &= 0x7fffffffffffffff
			topx &= 0x7ff
		}
		if (topy&0x7ff)-0x3be >= 0x43e-0x3be {
			if ix == asuint64(1.0) {
				return 1.0
			}
			if topy&0x7ff < 0x3be {
				if ix > asuint64(1.0) {
					return 1.0 + y
				}
				return 1.0 - y
			}
			if (ix > asuint64(1.0)) == (topy < 0x800) {
				return mathOflow(0)
			}
			return mathUflow(0)
		}
		if topx == 0 {
			ix = asuint64(float64(x * 0x1p52))
			ix &= 0x7fffffffffffffff
			ix -= 52 << 52
		}
	}
	hi, lo := powLogInline(ix, m.fma)
	var ehi, elo float64
	if m.fma {
		ehi = float64(y * hi)
		elo = float64(y*lo) + math.FMA(y, hi, -ehi)
	} else {
		yhi := asdouble(iy & mask27)
		ylo := y - yhi
		lhi := asdouble(asuint64(hi) & mask27)
		llo := hi - lhi + lo
		ehi = float64(yhi * lhi)
		elo = float64(ylo*lhi) + float64(y*llo)
	}
	return powExpInline(ehi, elo, signBias)
}

// trigonometry

func kernelCos(x, y float64) float64 {
	const (
		C1 float64 = 4.16666666666666019037e-02
		C2 float64 = -1.38888888888741095749e-03
		C3 float64 = 2.48015872894767294178e-05
		C4 float64 = -2.75573143513906633035e-07
		C5 float64 = 2.08757232129817482790e-09
		C6 float64 = -1.13596475577881948265e-11
	)
	z := float64(x * x)
	w := float64(z * z)
	r := float64(z*(C1+float64(z*(C2+float64(z*C3))))) + float64(float64(w*w)*(C4+float64(z*(C5+float64(z*C6)))))
	hz := float64(0.5 * z)
	w = 1.0 - hz
	return w + (((1.0 - w) - hz) + (float64(z*r) - float64(x*y)))
}

func kernelSin(x, y float64, iy int) float64 {
	const (
		S1 float64 = -1.66666666666666324348e-01
		S2 float64 = 8.33333333332248946124e-03
		S3 float64 = -1.98412698298579493134e-04
		S4 float64 = 2.75573137070700676789e-06
		S5 float64 = -2.50507602534068634195e-08
		S6 float64 = 1.58969099521155010221e-10
	)
	z := float64(x * x)
	w := float64(z * z)
	r := S2 + float64(z*(S3+float64(z*S4))) + float64(float64(z*w)*(S5+float64(z*S6)))
	v := float64(z * x)
	if iy == 0 {
		return x + float64(v*(S1+float64(z*r)))
	}
	return x - ((float64(z*(float64(0.5*y)-float64(v*r))) - y) - float64(v*S1))
}

var tanT = [...]float64{
	3.33333333333334091986e-01, 1.33333333333201242699e-01, 5.39682539762260521377e-02,
	2.18694882948595424599e-02, 8.86323982359930005737e-03, 3.59207910759131235356e-03,
	1.45620945432529025516e-03, 5.88041240820264096874e-04, 2.46463134818469906812e-04,
	7.81794442939557092300e-05, 7.14072491382608190305e-05, -1.85586374855275456654e-05,
	2.59073051863633712884e-05,
}

func kernelTan(x, y float64, odd int) float64 {
	const (
		pio4   float64 = 7.85398163397448278999e-01
		pio4lo float64 = 3.06161699786838301793e-17
	)
	T := &tanT
	hx := highWord(x)
	big := hx&0x7fffffff >= 0x3FE59428
	var sign uint32
	if big {
		sign = hx >> 31
		if sign != 0 {
			x = -x
			y = -y
		}
		x = (pio4 - x) + (pio4lo - y)
		y = 0.0
	}
	z := float64(x * x)
	w := float64(z * z)
	r := T[1] + float64(w*(T[3]+float64(w*(T[5]+float64(w*(T[7]+float64(w*(T[9]+float64(w*T[11])))))))))
	v := float64(z * (T[2] + float64(w*(T[4]+float64(w*(T[6]+float64(w*(T[8]+float64(w*(T[10]+float64(w*T[12])))))))))))
	s := float64(z * x)
	r = y + float64(z*(float64(s*(r+v))+y)) + float64(s*T[0])
	w = x + r
	if big {
		s = float64(1 - 2*odd)
		v = s - float64(2.0*(x+(r-float64(w*w)/(w+s))))
		if sign != 0 {
			return -v
		}
		return v
	}
	if odd == 0 {
		return w
	}
	w0 := withLowWord(w, 0)
	v = r - (w0 - x)
	a := -1.0 / w
	a0 := withLowWord(a, 0)
	return a0 + float64(a*(1.0+float64(a0*w0)+float64(a0*v)))
}

func remPio2(x float64) (int, float64, float64) {
	const (
		toint   float64 = 1.5 / 2.220446049250313e-16
		pio4    float64 = 0x1.921fb54442d18p-1
		invpio2 float64 = 6.36619772367581382433e-01
		pio2_1  float64 = 1.57079632673412561417e+00
		pio2_1t float64 = 6.07710050650619224932e-11
		pio2_2  float64 = 6.07710050630396597660e-11
		pio2_2t float64 = 2.02226624879595063154e-21
		pio2_3  float64 = 2.02226624871116645580e-21
		pio2_3t float64 = 8.47842766036889956997e-32
	)
	u := asuint64(x)
	sign := u >> 63
	ix := uint32(u>>32) & 0x7fffffff
	small := func(m float64, n int) (int, float64, float64) {
		// m*pio2_1 and m*pio2_1t: each one rounding, as C's runtime products.
		p, pt := float64(m*pio2_1), float64(m*pio2_1t)
		if sign == 0 {
			z := x - p
			y0 := z - pt
			return n, y0, (z - y0) - pt
		}
		z := x + p
		y0 := z + pt
		return -n, y0, (z - y0) + pt
	}
	medium := false
	switch {
	case ix <= 0x400f6a7a:
		if ix&0xfffff == 0x921fb {
			medium = true
		} else if ix <= 0x4002d97c {
			return small(1, 1)
		} else {
			return small(2, 2)
		}
	case ix <= 0x401c463b:
		if ix <= 0x4015fdbc {
			if ix == 0x4012d97c {
				medium = true
			} else {
				return small(3, 3)
			}
		} else {
			if ix == 0x401921fb {
				medium = true
			} else {
				return small(4, 4)
			}
		}
	case ix < 0x413921fb:
		medium = true
	}
	if medium {
		fn := float64(x*invpio2) + toint - toint
		n := int(int32(fn))
		r := x - float64(fn*pio2_1)
		w := float64(fn * pio2_1t)
		if r-w < -pio4 {
			n--
			fn--
			r = x - float64(fn*pio2_1)
			w = float64(fn * pio2_1t)
		} else if r-w > pio4 {
			n++
			fn++
			r = x - float64(fn*pio2_1)
			w = float64(fn * pio2_1t)
		}
		y0 := r - w
		ey := int(asuint64(y0) >> 52 & 0x7ff)
		ex := int(ix >> 20)
		if ex-ey > 16 {
			t := r
			w = float64(fn * pio2_2)
			r = t - w
			w = float64(fn*pio2_2t) - ((t - r) - w)
			y0 = r - w
			ey = int(asuint64(y0) >> 52 & 0x7ff)
			if ex-ey > 49 {
				t = r
				w = float64(fn * pio2_3)
				r = t - w
				w = float64(fn*pio2_3t) - ((t - r) - w)
				y0 = r - w
			}
		}
		return n, y0, (r - y0) - w
	}
	if ix >= 0x7ff00000 {
		y := x - x
		return 0, y, y
	}
	u = asuint64(x)
	u &= ^uint64(0) >> 12
	u |= uint64(0x3ff+23) << 52
	z := asdouble(u)
	var tx [3]float64
	i := 0
	for ; i < 2; i++ {
		tx[i] = float64(int32(z))
		z = float64((z - tx[i]) * 0x1p24)
	}
	tx[i] = z
	for tx[i] == 0.0 {
		i--
	}
	var ty [3]float64
	n := remPio2Large(tx[:], ty[:], int(ix>>20)-(0x3ff+23), i+1, 1)
	if sign != 0 {
		return -n, -ty[0], -ty[1]
	}
	return n, ty[0], ty[1]
}

var ipio2 = [...]int32{
	0xA2F983, 0x6E4E44, 0x1529FC, 0x2757D1, 0xF534DD, 0xC0DB62,
	0x95993C, 0x439041, 0xFE5163, 0xABDEBB, 0xC561B7, 0x246E3A,
	0x424DD2, 0xE00649, 0x2EEA09, 0xD1921C, 0xFE1DEB, 0x1CB129,
	0xA73EE8, 0x8235F5, 0x2EBB44, 0x84E99C, 0x7026B4, 0x5F7E41,
	0x3991D6, 0x398353, 0x39F49C, 0x845F8B, 0xBDF928, 0x3B1FF8,
	0x97FFDE, 0x05980F, 0xEF2F11, 0x8B5A0A, 0x6D1F6D, 0x367ECF,
	0x27CB09, 0xB74F46, 0x3F669E, 0x5FEA2D, 0x7527BA, 0xC7EBE5,
	0xF17B3D, 0x0739F7, 0x8A5292, 0xEA6BFB, 0x5FB11F, 0x8D5D08,
	0x560330, 0x46FC7B, 0x6BABF0, 0xCFBC20, 0x9AF436, 0x1DA9E3,
	0x91615E, 0xE61B08, 0x659985, 0x5F14A0, 0x68408D, 0xFFD880,
	0x4D7327, 0x310606, 0x1556CA, 0x73A8C9, 0x60E27B, 0xC08C6B,
}

var pio2Parts = [...]float64{
	1.57079625129699707031e+00, 7.54978941586159635335e-08, 5.39030252995776476554e-15,
	3.28200341580791294123e-22, 1.27065575308067607349e-29, 1.22933308981111328932e-36,
	2.73370053816464559624e-44, 2.16741683877804819444e-51,
}

func remPio2Large(x, y []float64, e0, nx, prec int) int {
	initJk := [...]int{3, 4, 4, 6}
	var iq [20]int32
	var f, fq, q [20]float64
	jk := initJk[prec]
	jp := jk
	jx := nx - 1
	jv := (e0 - 3) / 24
	if jv < 0 {
		jv = 0
	}
	q0 := e0 - 24*(jv+1)
	j := jv - jx
	m := jx + jk
	for i := 0; i <= m; i, j = i+1, j+1 {
		if j < 0 {
			f[i] = 0.0
		} else {
			f[i] = float64(ipio2[j])
		}
	}
	for i := 0; i <= jk; i++ {
		fw := 0.0
		for j := 0; j <= jx; j++ {
			fw += float64(x[j] * f[jx+i-j])
		}
		q[i] = fw
	}
	jz := jk
	var z float64
	var n, ih int32
recompute:
	{
		i := 0
		z = q[jz]
		for j := jz; j > 0; i, j = i+1, j-1 {
			fw := float64(int32(float64(0x1p-24 * z)))
			iq[i] = int32(z - float64(0x1p24*fw))
			z = q[j-1] + fw
		}
	}
	z = scalbn(z, q0)
	z -= float64(8.0 * math.Floor(float64(z*0.125)))
	n = int32(z)
	z -= float64(n)
	ih = 0
	if q0 > 0 {
		i := iq[jz-1] >> (24 - q0)
		n += i
		iq[jz-1] -= i << (24 - q0)
		ih = iq[jz-1] >> (23 - q0)
	} else if q0 == 0 {
		ih = iq[jz-1] >> 23
	} else if z >= 0.5 {
		ih = 2
	}
	if ih > 0 {
		n++
		carry := int32(0)
		for i := 0; i < jz; i++ {
			j := iq[i]
			if carry == 0 {
				if j != 0 {
					carry = 1
					iq[i] = 0x1000000 - j
				}
			} else {
				iq[i] = 0xffffff - j
			}
		}
		if q0 > 0 {
			switch q0 {
			case 1:
				iq[jz-1] &= 0x7fffff
			case 2:
				iq[jz-1] &= 0x3fffff
			}
		}
		if ih == 2 {
			z = 1.0 - z
			if carry != 0 {
				z -= scalbn(1.0, q0)
			}
		}
	}
	if z == 0.0 {
		j := int32(0)
		for i := jz - 1; i >= jk; i-- {
			j |= iq[i]
		}
		if j == 0 {
			k := 1
			for iq[jk-k] == 0 {
				k++
			}
			for i := jz + 1; i <= jz+k; i++ {
				f[jx+i] = float64(ipio2[jv+i])
				fw := 0.0
				for j := 0; j <= jx; j++ {
					fw += float64(x[j] * f[jx+i-j])
				}
				q[i] = fw
			}
			jz += k
			goto recompute
		}
	}
	if z == 0.0 {
		jz--
		q0 -= 24
		for iq[jz] == 0 {
			jz--
			q0 -= 24
		}
	} else {
		z = scalbn(z, -q0)
		if z >= 0x1p24 {
			fw := float64(int32(float64(0x1p-24 * z)))
			iq[jz] = int32(z - float64(0x1p24*fw))
			jz++
			q0 += 24
			iq[jz] = int32(fw)
		} else {
			iq[jz] = int32(z)
		}
	}
	fw := scalbn(1.0, q0)
	for i := jz; i >= 0; i-- {
		q[i] = float64(fw * float64(iq[i]))
		fw = float64(fw * 0x1p-24)
	}
	for i := jz; i >= 0; i-- {
		fw := 0.0
		for k := 0; k <= jp && k <= jz-i; k++ {
			fw += float64(pio2Parts[k] * q[i+k])
		}
		fq[jz-i] = fw
	}
	// prec 1: two doubles of the result.
	fw = 0.0
	for i := jz; i >= 0; i-- {
		fw += fq[i]
	}
	if ih == 0 {
		y[0] = fw
	} else {
		y[0] = -fw
	}
	fw = fq[0] - fw
	for i := 1; i <= jz; i++ {
		fw += fq[i]
	}
	if ih == 0 {
		y[1] = fw
	} else {
		y[1] = -fw
	}
	return int(n & 7)
}

func scalbn(x float64, n int) float64 {
	y := x
	if n > 1023 {
		y = float64(y * 0x1p1023)
		n -= 1023
		if n > 1023 {
			y = float64(y * 0x1p1023)
			n -= 1023
			if n > 1023 {
				n = 1023
			}
		}
	} else if n < -1022 {
		y = float64(y * (0x1p-1022 * 0x1p53))
		n += 1022 - 53
		if n < -1022 {
			y = float64(y * (0x1p-1022 * 0x1p53))
			n += 1022 - 53
			if n < -1022 {
				n = -1022
			}
		}
	}
	return float64(y * asdouble(uint64(0x3ff+n)<<52))
}

func (m muslLibm) Cos(x float64) float64 {
	ix := highWord(x) & 0x7fffffff
	if ix <= 0x3fe921fb {
		if ix < 0x3e46a09e {
			return 1.0
		}
		return kernelCos(x, 0)
	}
	if ix >= 0x7ff00000 {
		return x - x
	}
	n, y0, y1 := remPio2(x)
	switch n & 3 {
	case 0:
		return kernelCos(y0, y1)
	case 1:
		return -kernelSin(y0, y1, 1)
	case 2:
		return -kernelCos(y0, y1)
	}
	return kernelSin(y0, y1, 1)
}

func (m muslLibm) Sin(x float64) float64 {
	ix := highWord(x) & 0x7fffffff
	if ix <= 0x3fe921fb {
		if ix < 0x3e500000 {
			return x
		}
		return kernelSin(x, 0.0, 0)
	}
	if ix >= 0x7ff00000 {
		return x - x
	}
	n, y0, y1 := remPio2(x)
	switch n & 3 {
	case 0:
		return kernelSin(y0, y1, 1)
	case 1:
		return kernelCos(y0, y1)
	case 2:
		return -kernelSin(y0, y1, 1)
	}
	return -kernelCos(y0, y1)
}

func (m muslLibm) Tan(x float64) float64 {
	ix := highWord(x) & 0x7fffffff
	if ix <= 0x3fe921fb {
		if ix < 0x3e400000 {
			return x
		}
		return kernelTan(x, 0.0, 0)
	}
	if ix >= 0x7ff00000 {
		return x - x
	}
	n, y0, y1 := remPio2(x)
	return kernelTan(y0, y1, n&1)
}

// inverse trigonometry

const (
	pio2Hi float64 = 1.57079632679489655800e+00
	pio2Lo float64 = 6.12323399573676603587e-17
)

func asinR(z float64) float64 {
	const (
		pS0 float64 = 1.66666666666666657415e-01
		pS1 float64 = -3.25565818622400915405e-01
		pS2 float64 = 2.01212532134862925881e-01
		pS3 float64 = -4.00555345006794114027e-02
		pS4 float64 = 7.91534994289814532176e-04
		pS5 float64 = 3.47933107596021167570e-05
		qS1 float64 = -2.40339491173441421878e+00
		qS2 float64 = 2.02094576023350569471e+00
		qS3 float64 = -6.88283971605453293030e-01
		qS4 float64 = 7.70381505559019352791e-02
	)
	p := float64(z * (pS0 + float64(z*(pS1+float64(z*(pS2+float64(z*(pS3+float64(z*(pS4+float64(z*pS5)))))))))))
	q := 1.0 + float64(z*(qS1+float64(z*(qS2+float64(z*(qS3+float64(z*qS4)))))))
	return p / q
}

func (m muslLibm) Asin(x float64) float64 {
	hx := highWord(x)
	ix := hx & 0x7fffffff
	if ix >= 0x3ff00000 {
		lx := lowWord(x)
		if (ix-0x3ff00000)|lx == 0 {
			return float64(x*pio2Hi) + 0x1p-120
		}
		return 0 / (x - x)
	}
	if ix < 0x3fe00000 {
		if ix < 0x3e500000 && ix >= 0x00100000 {
			return x
		}
		return x + float64(x*asinR(float64(x*x)))
	}
	z := float64((1 - math.Abs(x)) * 0.5)
	s := math.Sqrt(z)
	r := asinR(z)
	if ix >= 0x3fef3333 {
		x = pio2Hi - (float64(2*(s+float64(s*r))) - pio2Lo)
	} else {
		f := withLowWord(s, 0)
		c := (z - float64(f*f)) / (s + f)
		x = float64(0.5*pio2Hi) - (float64(float64(2*s)*r) - (pio2Lo - float64(2*c)) - (float64(0.5*pio2Hi) - float64(2*f)))
	}
	if hx>>31 != 0 {
		return -x
	}
	return x
}

func (m muslLibm) Acos(x float64) float64 {
	hx := highWord(x)
	ix := hx & 0x7fffffff
	if ix >= 0x3ff00000 {
		lx := lowWord(x)
		if (ix-0x3ff00000)|lx == 0 {
			if hx>>31 != 0 {
				return float64(2*pio2Hi) + 0x1p-120
			}
			return 0
		}
		return 0 / (x - x)
	}
	if ix < 0x3fe00000 {
		if ix <= 0x3c600000 {
			return pio2Hi + 0x1p-120
		}
		return pio2Hi - (x - (pio2Lo - float64(x*asinR(float64(x*x)))))
	}
	if hx>>31 != 0 {
		z := float64((1.0 + x) * 0.5)
		s := math.Sqrt(z)
		w := float64(asinR(z)*s) - pio2Lo
		return float64(2 * (pio2Hi - (s + w)))
	}
	z := float64((1.0 - x) * 0.5)
	s := math.Sqrt(z)
	df := withLowWord(s, 0)
	c := (z - float64(df*df)) / (s + df)
	w := float64(asinR(z)*s) + c
	return float64(2 * (df + w))
}

var (
	atanHi = [...]float64{4.63647609000806093515e-01, 7.85398163397448278999e-01, 9.82793723247329054082e-01, 1.57079632679489655800e+00}
	atanLo = [...]float64{2.26987774529616870924e-17, 3.06161699786838301793e-17, 1.39033110312309984516e-17, 6.12323399573676603587e-17}
	aT     = [...]float64{
		3.33333333333329318027e-01, -1.99999999998764832476e-01, 1.42857142725034663711e-01,
		-1.11111104054623557880e-01, 9.09088713343650656196e-02, -7.69187620504482999495e-02,
		6.66107313738753120669e-02, -5.83357013379057348645e-02, 4.97687799461593236017e-02,
		-3.65315727442169155270e-02, 1.62858201153657823623e-02,
	}
)

func (m muslLibm) Atan(x float64) float64 {
	ix := highWord(x)
	sign := ix >> 31
	ix &= 0x7fffffff
	if ix >= 0x44100000 {
		if math.IsNaN(x) {
			return x
		}
		z := atanHi[3] + 0x1p-120
		if sign != 0 {
			return -z
		}
		return z
	}
	var id int
	if ix < 0x3fdc0000 {
		if ix < 0x3e400000 {
			return x
		}
		id = -1
	} else {
		x = math.Abs(x)
		if ix < 0x3ff30000 {
			if ix < 0x3fe60000 {
				id = 0
				x = (float64(2.0*x) - 1.0) / (2.0 + x)
			} else {
				id = 1
				x = (x - 1.0) / (x + 1.0)
			}
		} else {
			if ix < 0x40038000 {
				id = 2
				x = (x - 1.5) / (1.0 + float64(1.5*x))
			} else {
				id = 3
				x = -1.0 / x
			}
		}
	}
	z := float64(x * x)
	w := float64(z * z)
	s1 := float64(z * (aT[0] + float64(w*(aT[2]+float64(w*(aT[4]+float64(w*(aT[6]+float64(w*(aT[8]+float64(w*aT[10])))))))))))
	s2 := float64(w * (aT[1] + float64(w*(aT[3]+float64(w*(aT[5]+float64(w*(aT[7]+float64(w*aT[9])))))))))
	if id < 0 {
		return x - float64(x*(s1+s2))
	}
	z = atanHi[id] - ((float64(x*(s1+s2)) - atanLo[id]) - x)
	if sign != 0 {
		return -z
	}
	return z
}

func (l muslLibm) Atan2(y, x float64) float64 {
	const (
		pi   float64 = 3.1415926535897931160e+00
		piLo float64 = 1.2246467991473531772e-16
	)
	if math.IsNaN(x) || math.IsNaN(y) {
		return x + y
	}
	ix, lx := highWord(x), lowWord(x)
	iy, ly := highWord(y), lowWord(y)
	if (ix-0x3ff00000)|lx == 0 {
		return l.Atan(y)
	}
	m := ((iy >> 31) & 1) | ((ix >> 30) & 2)
	ix &= 0x7fffffff
	iy &= 0x7fffffff
	if iy|ly == 0 {
		switch m {
		case 0, 1:
			return y
		case 2:
			return pi
		case 3:
			return -pi
		}
	}
	if ix|lx == 0 {
		if m&1 != 0 {
			return -pi / 2
		}
		return pi / 2
	}
	if ix == 0x7ff00000 {
		if iy == 0x7ff00000 {
			switch m {
			case 0:
				return pi / 4
			case 1:
				return -pi / 4
			case 2:
				return float64(3*pi) / 4
			case 3:
				return float64(-3*pi) / 4
			}
		} else {
			switch m {
			case 0:
				return 0.0
			case 1:
				return math.Copysign(0, -1)
			case 2:
				return pi
			case 3:
				return -pi
			}
		}
	}
	if ix+(64<<20) < iy || iy == 0x7ff00000 {
		if m&1 != 0 {
			return -pi / 2
		}
		return pi / 2
	}
	var z float64
	if m&2 != 0 && iy+(64<<20) < ix {
		z = 0
	} else {
		z = l.Atan(math.Abs(y / x))
	}
	switch m {
	case 0:
		return z
	case 1:
		return -z
	case 2:
		return pi - (z - piLo)
	}
	return (z - piLo) - pi
}

// hyperbolic

func muslExpm1(x float64) float64 {
	const (
		oThreshold float64 = 7.09782712893383973096e+02
		ln2Hi      float64 = 6.93147180369123816490e-01
		ln2Lo      float64 = 1.90821492927058770002e-10
		invln2     float64 = 1.44269504088896338700e+00
		Q1         float64 = -3.33333333333331316428e-02
		Q2         float64 = 1.58730158725481460165e-03
		Q3         float64 = -7.93650757867487942473e-05
		Q4         float64 = 4.00821782732936239552e-06
		Q5         float64 = -2.01099218183624371326e-07
	)
	u := asuint64(x)
	hx := uint32(u>>32) & 0x7fffffff
	sign := u >> 63
	var k int
	var hi, lo, c float64
	if hx >= 0x4043687A {
		if math.IsNaN(x) {
			return x
		}
		if sign != 0 {
			return -1
		}
		if x > oThreshold {
			return float64(x * 0x1p1023)
		}
	}
	if hx > 0x3fd62e42 {
		if hx < 0x3FF0A2B2 {
			if sign == 0 {
				hi = x - ln2Hi
				lo = ln2Lo
				k = 1
			} else {
				hi = x + ln2Hi
				lo = -ln2Lo
				k = -1
			}
		} else {
			half := 0.5
			if sign != 0 {
				half = -0.5
			}
			k = int(float64(invln2*x) + half)
			t := float64(k)
			hi = x - float64(t*ln2Hi)
			lo = float64(t * ln2Lo)
		}
		x = hi - lo
		c = (hi - x) - lo
	} else if hx < 0x3c900000 {
		return x
	} else {
		k = 0
	}
	hfx := float64(0.5 * x)
	hxs := float64(x * hfx)
	r1 := 1.0 + float64(hxs*(Q1+float64(hxs*(Q2+float64(hxs*(Q3+float64(hxs*(Q4+float64(hxs*Q5)))))))))
	t := 3.0 - float64(r1*hfx)
	e := float64(hxs * ((r1 - t) / (6.0 - float64(x*t))))
	if k == 0 {
		return x - (float64(x*e) - hxs)
	}
	e = float64(x*(e-c)) - c
	e -= hxs
	if k == -1 {
		return float64(0.5*(x-e)) - 0.5
	}
	if k == 1 {
		if x < -0.25 {
			return float64(-2.0 * (e - (x + 0.5)))
		}
		return 1.0 + float64(2.0*(x-e))
	}
	twopk := asdouble(uint64(0x3ff+k) << 52)
	if k < 0 || k > 56 {
		y := x - e + 1.0
		if k == 1024 {
			y = float64(float64(y*2.0) * 0x1p1023)
		} else {
			y = float64(y * twopk)
		}
		return y - 1.0
	}
	uf := asdouble(uint64(0x3ff-k) << 52)
	if k < 20 {
		return float64((x - e + (1 - uf)) * twopk)
	}
	return float64((x - (e + uf) + 1) * twopk)
}

func (m muslLibm) expo2(x, sign float64) float64 {
	const k = 2043
	const kln2 float64 = 0x1.62066151add8bp+10
	scale := asdouble(uint64(uint32(0x3ff+k/2)<<20) << 32)
	return float64(float64(m.Exp(x-kln2)*float64(sign*scale)) * scale)
}

func (m muslLibm) Sinh(x float64) float64 {
	u := asuint64(x)
	h := 0.5
	if u>>63 != 0 {
		h = -h
	}
	u &= ^uint64(0) / 2
	absx := asdouble(u)
	w := uint32(u >> 32)
	if w < 0x40862e42 {
		t := muslExpm1(absx)
		if w < 0x3ff00000 {
			if w < 0x3ff00000-(26<<20) {
				return x
			}
			return float64(h * (float64(2*t) - float64(t*t)/(t+1)))
		}
		return float64(h * (t + t/(t+1)))
	}
	return m.expo2(absx, float64(2*h))
}

func (m muslLibm) Cosh(x float64) float64 {
	u := asuint64(x)
	u &= ^uint64(0) / 2
	x = asdouble(u)
	w := uint32(u >> 32)
	if w < 0x3fe62e42 {
		if w < 0x3ff00000-(26<<20) {
			return 1
		}
		t := muslExpm1(x)
		return 1 + float64(t*t)/float64(2*(1+t))
	}
	if w < 0x40862e42 {
		t := m.Exp(x)
		return float64(0.5 * (t + 1/t))
	}
	return m.expo2(x, 1.0)
}

func (m muslLibm) Tanh(x float64) float64 {
	u := asuint64(x)
	sign := u >> 63
	u &= ^uint64(0) / 2
	x = asdouble(u)
	w := uint32(u >> 32)
	var t float64
	switch {
	case w > 0x3fe193ea:
		if w > 0x40340000 {
			t = 1 - 0/x
		} else {
			t = muslExpm1(float64(2 * x))
			t = 1 - 2/(t+2)
		}
	case w > 0x3fd058ae:
		t = muslExpm1(float64(2 * x))
		t = t / (t + 2)
	case w >= 0x00100000:
		t = muslExpm1(float64(-2 * x))
		t = -t / (t + 2)
	default:
		t = x
	}
	if sign != 0 {
		return -t
	}
	return t
}

const ln2Long float64 = 0.693147180559945309417232121458176568

func (m muslLibm) Asinh(x float64) float64 {
	u := asuint64(x)
	e := uint32(u >> 52 & 0x7ff)
	s := u >> 63
	u &= ^uint64(0) / 2
	x = asdouble(u)
	switch {
	case e >= 0x3ff+26:
		x = m.Log(x) + ln2Long
	case e >= 0x3ff+1:
		x = m.Log(float64(2*x) + 1/(math.Sqrt(float64(x*x)+1)+x))
	case e >= 0x3ff-26:
		x = muslLog1p(x + float64(x*x)/(math.Sqrt(float64(x*x)+1)+1))
	}
	if s != 0 {
		return -x
	}
	return x
}

func (m muslLibm) Acosh(x float64) float64 {
	e := uint32(asuint64(x) >> 52 & 0x7ff)
	if e < 0x3ff+1 {
		return muslLog1p(x - 1 + math.Sqrt(float64((x-1)*(x-1))+float64(2*(x-1))))
	}
	if e < 0x3ff+26 {
		return m.Log(float64(2*x) - 1/(x+math.Sqrt(float64(x*x)-1)))
	}
	return m.Log(x) + ln2Long
}

func (m muslLibm) Atanh(x float64) float64 {
	u := asuint64(x)
	e := uint32(u >> 52 & 0x7ff)
	s := u >> 63
	u &= ^uint64(0) / 2
	y := asdouble(u)
	if e < 0x3ff-1 {
		if e >= 0x3ff-32 {
			y = float64(0.5 * muslLog1p(float64(2*y)+float64(float64(2*y)*y)/(1-y)))
		}
	} else {
		y = float64(0.5 * muslLog1p(float64(2*(y/(1-y)))))
	}
	if s != 0 {
		return -y
	}
	return y
}

// Masks clearing a double's low 32 and 27 bits (C's -1ULL << 32 and << 27).
const (
	mask32 uint64 = 0xffffffff00000000
	mask27 uint64 = 0xfffffffff8000000
)

// Cbrt is musl's cbrt.
func (m muslLibm) Cbrt(x float64) float64 {
	const (
		B1 uint32  = 715094163
		B2 uint32  = 696219795
		P0 float64 = 1.87595182427177009643
		P1 float64 = -1.88497979543377169875
		P2 float64 = 1.621429720105354466140
		P3 float64 = -0.758397934778766047437
		P4 float64 = 0.145996192886612446982
	)
	u := asuint64(x)
	hx := uint32(u>>32) & 0x7fffffff
	if hx >= 0x7ff00000 {
		return x + x
	}
	if hx < 0x00100000 {
		u = asuint64(float64(x * 0x1p54))
		hx = uint32(u>>32) & 0x7fffffff
		if hx == 0 {
			return x
		}
		hx = hx/3 + B2
	} else {
		hx = hx/3 + B1
	}
	u &= 1 << 63
	u |= uint64(hx) << 32
	t := asdouble(u)
	r := float64(float64(t*t) * (t / x))
	t = float64(t * ((P0 + float64(r*(P1+float64(r*P2)))) + float64(float64(float64(r*r)*r)*(P3+float64(r*P4)))))
	u = (asuint64(t) + 0x80000000) & 0xffffffffc0000000
	t = asdouble(u)
	s := float64(t * t)
	r = x / s
	w := t + t
	r = (r - t) / (w + r)
	return t + float64(t*r)
}
