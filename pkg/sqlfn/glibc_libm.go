package sqlfn

import "math"

// glibc 2.31's sin, cos and cbrt (sysdeps/ieee754/dbl-64: s_sin.c, branred.c, s_cbrt.c), the C
// library of stackql's Postgres image. Its sin and cos are within 0.55 ulp rather than correctly
// rounded, so only their own algorithm agrees with them. GCC builds glibc with floating-point
// contraction: a product whose every use is an addition or subtraction fuses into each, as fma here;
// any other product is rounded on its own. That is the aarch64 build, and x86_64's FMA variants of
// sin and cos; x86_64's cbrt is built without contraction and can differ from this in the last bit.

var fma = math.FMA

const (
	gS1    float64 = -0x1.5555555555555p-3
	gS2    float64 = 0x1.1111111110ECEp-7
	gS3    float64 = -0x1.A01A019DB08B8p-13
	gS4    float64 = 0x1.71DE27B9A7ED9p-19
	gS5    float64 = -0x1.ADDFFC2FCDF59p-26
	gBig   float64 = 0x1.8000000000000p45
	gHp0   float64 = 0x1.921FB54442D18p0
	gHp1   float64 = 0x1.1A62633145C07p-54
	gMp1   float64 = 0x1.921FB58000000p0
	gMp2   float64 = -0x1.DDE973C000000p-27
	gPp3   float64 = -0x1.CB3B398000000p-55
	gPp4   float64 = -0x1.d747f23e32ed7p-83
	gHpinv float64 = 0x1.45F306DC9C883p-1
	gToint float64 = 0x1.8000000000000p52

	gSn3 float64 = -1.66666666666664880952546298448555e-01
	gSn5 float64 = 8.33333214285722277379541354343671e-03
	gCs2 float64 = 4.99999999999999999999950396842453e-01
	gCs4 float64 = -4.16666666666664434524222570944589e-02
	gCs6 float64 = 1.38888874007937613028114285595617e-03
)

func lowHalf(x float64) uint32  { return uint32(math.Float64bits(x)) }
func highHalf(x float64) uint32 { return uint32(math.Float64bits(x) >> 32) }

func sincosLookup(u float64) (sn, ssn, cs, ccs float64) {
	k := lowHalf(u) << 2
	t := &glibcSinCosTab
	return math.Float64frombits(t[k]), math.Float64frombits(t[k+1]), math.Float64frombits(t[k+2]), math.Float64frombits(t[k+3])
}

func taylorSin(xx, a, da float64) float64 {
	poly := fma(fma(fma(fma(gS5, xx, gS4), xx, gS3), xx, gS2), xx, gS1)
	t := fma(fma(poly, a, -float64(0.5*da)), xx, da)
	return a + t
}

func glibcDoCos(x, dx float64) float64 {
	if x < 0 {
		dx = -dx
	}
	u := gBig + math.Abs(x)
	x = math.Abs(x) - (u - gBig) + dx
	xx := float64(x * x)
	s := fma(float64(x*xx), fma(xx, gSn5, gSn3), x)
	c := float64(xx * fma(xx, fma(xx, gCs6, gCs4), gCs2))
	sn, ssn, cs, ccs := sincosLookup(u)
	cor := fma(-sn, s, fma(-cs, c, fma(-s, ssn, ccs)))
	return cs + cor
}

func glibcDoSin(x, dx float64) float64 {
	xold := x
	if math.Abs(x) < 0.126 {
		return taylorSin(float64(x*x), x, dx)
	}
	if x <= 0 {
		dx = -dx
	}
	u := gBig + math.Abs(x)
	x = math.Abs(x) - (u - gBig)
	xx := float64(x * x)
	s := x + fma(float64(x*xx), fma(xx, gSn5, gSn3), dx)
	c := fma(x, dx, float64(xx*fma(xx, fma(xx, gCs6, gCs4), gCs2)))
	sn, ssn, cs, ccs := sincosLookup(u)
	cor := fma(cs, s, fma(-sn, c, fma(s, ccs, ssn)))
	return math.Copysign(sn+cor, xold)
}

func glibcReduce(x float64) (int, float64, float64) {
	t := fma(x, gHpinv, gToint)
	xn := t - gToint
	y := fma(-xn, gMp2, fma(-xn, gMp1, x))
	n := int(lowHalf(t) & 3)
	t2 := fma(-xn, gPp3, y)
	db := fma(-xn, gPp3, y-t2)
	b := fma(-xn, gPp4, t2)
	db += fma(-xn, gPp4, t2-b)
	return n, b, db
}

func glibcDoSinCos(a, da float64, n int) float64 {
	var r float64
	if n&1 != 0 {
		r = glibcDoCos(a, da)
	} else {
		r = glibcDoSin(a, da)
	}
	if n&2 != 0 {
		return -r
	}
	return r
}

var glibcToverp = [75]float64{
	10680707.0, 7228996.0, 1387004.0, 2578385.0, 16069853.0,
	12639074.0, 9804092.0, 4427841.0, 16666979.0, 11263675.0,
	12935607.0, 2387514.0, 4345298.0, 14681673.0, 3074569.0,
	13734428.0, 16653803.0, 1880361.0, 10960616.0, 8533493.0,
	3062596.0, 8710556.0, 7349940.0, 6258241.0, 3772886.0,
	3769171.0, 3798172.0, 8675211.0, 12450088.0, 3874808.0,
	9961438.0, 366607.0, 15675153.0, 9132554.0, 7151469.0,
	3571407.0, 2607881.0, 12013382.0, 4155038.0, 6285869.0,
	7677882.0, 13102053.0, 15825725.0, 473591.0, 9065106.0,
	15363067.0, 6271263.0, 9264392.0, 5636912.0, 4652155.0,
	7056368.0, 13614112.0, 10155062.0, 1944035.0, 9527646.0,
	15080200.0, 6658437.0, 6231200.0, 6832269.0, 16767104.0,
	5075751.0, 3212806.0, 1398474.0, 7579849.0, 6349435.0,
	12618859.0, 4703257.0, 12806093.0, 14477321.0, 2786137.0,
	12875403.0, 9837734.0, 14528324.0, 13719321.0, 343717.0,
}

// glibcBranred is __branred: x reduced modulo pi/2, for |x| beyond about 1e8. glibc builds it
// without contraction, so every product here is rounded on its own.
func glibcBranred(x float64) (int, float64, float64) {
	const split = 134217729.0
	t576 := math.Float64frombits(0x63f0000000000000)
	tm600 := math.Float64frombits(0x1a70000000000000)
	tm24 := math.Float64frombits(0x3e70000000000000)
	big := math.Float64frombits(0x4338000000000000)
	big1 := math.Float64frombits(0x4358000000000000)
	// branred.h has its own mp1 and mp2, not usncs.h's.
	mp1 := math.Float64frombits(0x3FF921FB58000000)
	mp2 := math.Float64frombits(0xBE4DDE9740000000)
	x = float64(x * tm600)
	t := float64(x * split)
	x1 := t - (t - x)
	x2 := x - x1
	part := func(xp float64) (b, bb, sum float64) {
		var r [6]float64
		k := int((highHalf(xp) >> 20) & 2047)
		k = (k - 450) / 24
		if k < 0 {
			k = 0
		}
		gor := math.Float64frombits(math.Float64bits(t576) - uint64(k*24)<<52)
		for i := 0; i < 6; i++ {
			r[i] = float64(float64(xp*glibcToverp[k+i]) * gor)
			gor = float64(gor * tm24)
		}
		for i := 0; i < 3; i++ {
			s := (r[i] + big) - big
			sum += s
			r[i] -= s
		}
		t := 0.0
		for i := 0; i < 6; i++ {
			t += r[5-i]
		}
		bb = (((((r[0] - t) + r[1]) + r[2]) + r[3]) + r[4]) + r[5]
		s := (t + big) - big
		sum += s
		t -= s
		b = t + bb
		bb = (t - b) + bb
		s = (sum + big1) - big1
		sum -= s
		return b, bb, sum
	}
	b1, bb1, sum1 := part(x1)
	b2, bb2, sum2 := part(x2)
	sum := sum1 + sum2
	b := b1 + b2
	var bb float64
	if math.Abs(b1) > math.Abs(b2) {
		bb = (b1 - b) + b2
	} else {
		bb = (b2 - b) + b1
	}
	if b > 0.5 {
		b -= 1.0
		sum += 1.0
	} else if b < -0.5 {
		b += 1.0
		sum -= 1.0
	}
	s := b + (bb + bb1 + bb2)
	t = ((b - s) + bb) + (bb1 + bb2)
	b = float64(s * split)
	t1 := b - (b - s)
	t2 := s - t1
	b = float64(s * gHp0)
	bb = (((float64(t1*mp1) - b) + float64(t1*mp2)) + float64(t2*mp1)) + (float64(t2*mp2) + float64(s*gHp1) + float64(t*gHp0))
	s = b + bb
	t = (b - s) + bb
	return int(sum) & 3, s, t
}

func glibcSin(x float64) float64 {
	k := highHalf(x) & 0x7fffffff
	switch {
	case k < 0x3e500000:
		return x
	case k < 0x3feb6000:
		return glibcDoSin(x, 0)
	case k < 0x400368fd:
		t := gHp0 - math.Abs(x)
		return math.Copysign(glibcDoCos(t, gHp1), x)
	case k < 0x419921FB:
		n, a, da := glibcReduce(x)
		return glibcDoSinCos(a, da, n)
	case k < 0x7ff00000:
		n, a, da := glibcBranred(x)
		return glibcDoSinCos(a, da, n)
	}
	return x / x
}

func glibcCos(x float64) float64 {
	k := highHalf(x) & 0x7fffffff
	switch {
	case k < 0x3e400000:
		return 1.0
	case k < 0x3feb6000:
		return glibcDoCos(x, 0)
	case k < 0x400368fd:
		y := gHp0 - math.Abs(x)
		a := y + gHp1
		da := (y - a) + gHp1
		return glibcDoSin(a, da)
	case k < 0x419921FB:
		n, a, da := glibcReduce(x)
		return glibcDoSinCos(a, da, n+1)
	case k < 0x7ff00000:
		n, a, da := glibcBranred(x)
		return glibcDoSinCos(a, da, n+1)
	}
	return x / x
}

// glibcCbrtFactor is 2^(n/3), n = -2..2, each a double quotient as C computes it (a Go constant
// quotient would be exact before rounding).
var glibcCbrtFactor = func() [5]float64 {
	cbrt2, sqrCbrt2 := 1.2599210498948731648, 1.5874010519681994748
	return [5]float64{1.0 / sqrCbrt2, 1.0 / cbrt2, 1.0, cbrt2, sqrCbrt2}
}()

// cbrt is glibc's __cbrt.
func (g glibcOuter) cbrt(x float64) float64 {
	if x == 0 || math.IsNaN(x) || math.IsInf(x, 0) {
		return x + x
	}
	xm, xe := math.Frexp(math.Abs(x))
	u := g.fma(g.fma(g.fma(g.fma(g.fma(g.fma(-0.145263899385486377, xm, 0.784932344976639262), xm, -1.83469277483613086), xm, 2.44693122563534430), xm, -2.11499494167371287), xm, 1.50819193781584896), xm, 0.354895765043919860)
	t2 := float64(float64(u*u) * u)
	ym := float64(float64(u*g.fma(2.0, xm, t2)) / g.fma(2.0, t2, xm) * glibcCbrtFactor[2+xe%3])
	if x <= 0 {
		ym = -ym
	}
	return math.Ldexp(ym, xe/3)
}

// expm1 is glibc's fdlibm __expm1.
func (g glibcOuter) expm1(x float64) float64 {
	const (
		oThreshold float64 = 7.09782712893383973096e+02
		ln2Hi      float64 = 6.93147180369123816490e-01
		ln2Lo      float64 = 1.90821492927058770002e-10
		invln2     float64 = 1.44269504088896338700e+00
		q1         float64 = -3.33333333333331316428e-02
		q2         float64 = 1.58730158725481460165e-03
		q3         float64 = -7.93650757867487942473e-05
		q4         float64 = 4.00821782732936239552e-06
		q5         float64 = -2.01099218183624371326e-07
	)
	hx := highHalf(x)
	neg := hx&0x80000000 != 0
	hx &= 0x7fffffff
	if hx >= 0x4043687A {
		if hx >= 0x40862E42 {
			if hx >= 0x7ff00000 {
				if (hx&0xfffff)|lowHalf(x) != 0 {
					return x + x
				}
				if !neg {
					return x
				}
				return -1.0
			}
			if x > oThreshold {
				return math.Inf(1)
			}
		}
		if neg {
			return 1e-300 - 1.0
		}
	}
	var k int32
	var hi, lo, c float64
	switch {
	case hx > 0x3fd62e42:
		if hx < 0x3FF0A2B2 {
			if !neg {
				hi, lo, k = x-ln2Hi, ln2Lo, 1
			} else {
				hi, lo, k = x+ln2Hi, -ln2Lo, -1
			}
		} else {
			half := 0.5
			if neg {
				half = -0.5
			}
			k = int32(g.fma(invln2, x, half))
			t := float64(k)
			hi = g.fma(-t, ln2Hi, x)
			// lo reaches its uses through the branches' join, so it is not fused.
			lo = float64(t * ln2Lo)
		}
		x = hi - lo
		c = (hi - x) - lo
	case hx < 0x3c900000:
		return x
	}
	hfx := float64(0.5 * x)
	hxs := float64(x * hfx)
	r1a := g.fma(hxs, q1, 1.0)
	h2 := float64(hxs * hxs)
	r2 := g.fma(hxs, q3, q2)
	h4 := float64(h2 * h2)
	r3 := g.fma(hxs, q5, q4)
	r1 := g.fma(h4, r3, g.fma(h2, r2, r1a))
	t := g.fma(-r1, hfx, 3.0)
	e := float64(hxs * ((r1 - t) / g.fma(-x, t, 6.0)))
	if k == 0 {
		return x - g.fma(x, e, -hxs)
	}
	e = g.fma(x, e-c, -c)
	e -= hxs
	if k == -1 {
		return g.fma(0.5, x-e, -0.5)
	}
	if k == 1 {
		if x < -0.25 {
			return float64(-2.0 * (e - (x + 0.5)))
		}
		return g.fma(2.0, x-e, 1.0)
	}
	addExp := func(y float64) float64 {
		return math.Float64frombits(uint64(highHalf(y)+uint32(k)<<20)<<32 | uint64(lowHalf(y)))
	}
	if k <= -2 || k > 56 {
		return addExp(1.0-(e-x)) - 1.0
	}
	if k < 20 {
		t := math.Float64frombits(uint64(0x3ff00000-(0x200000>>uint(k))) << 32)
		return addExp(t - (e - x))
	}
	t = math.Float64frombits(uint64((0x3ff-k)<<20) << 32)
	return addExp(x - (e + t) + 1.0)
}

// log1p is glibc's fdlibm __log1p.
func (g glibcOuter) log1p(x float64) float64 {
	const (
		ln2Hi float64 = 6.93147180369123816490e-01
		ln2Lo float64 = 1.90821492927058770002e-10
		lp1   float64 = 6.666666666666735130e-01
		lp2   float64 = 3.999999999940941908e-01
		lp3   float64 = 2.857142874366239149e-01
		lp4   float64 = 2.222219843214978396e-01
		lp5   float64 = 1.818357216161805012e-01
		lp6   float64 = 1.531383769920937332e-01
		lp7   float64 = 1.479819860511658591e-01
	)
	hx := int32(highHalf(x))
	ax := hx & 0x7fffffff
	k := int32(1)
	var f, c float64
	var hu int32
	if hx < 0x3FDA827A {
		if ax >= 0x3ff00000 {
			if x == -1.0 {
				return math.Inf(-1)
			}
			return math.NaN()
		}
		if ax < 0x3e200000 {
			if ax < 0x3c900000 {
				return x
			}
			return g.fma(-float64(x*x), 0.5, x)
		}
		if hx > 0 || hx <= int32(-1076707645) { // 0xbfd2bec3
			k, f, hu = 0, x, 1
		}
	} else if hx >= 0x7ff00000 {
		return x + x
	}
	var u float64
	if k != 0 {
		if hx < 0x43400000 {
			u = 1.0 + x
			hu = int32(highHalf(u))
			k = (hu >> 20) - 1023
			if k > 0 {
				c = 1.0 - (u - x)
			} else {
				c = x - (u - 1.0)
			}
			c /= u
		} else {
			u = x
			hu = int32(highHalf(u))
			k = (hu >> 20) - 1023
			c = 0
		}
		hu &= 0x000fffff
		setHigh := func(h int32) float64 {
			return math.Float64frombits(uint64(uint32(h))<<32 | uint64(lowHalf(u)))
		}
		if hu < 0x6a09e {
			u = setHigh(hu | 0x3ff00000)
		} else {
			k++
			u = setHigh(hu | 0x3fe00000)
			hu = (0x00100000 - hu) >> 2
		}
		f = u - 1.0
	}
	dk := float64(k)
	hfsq := float64(float64(0.5*f) * f)
	if hu == 0 {
		if f == 0 {
			if k == 0 {
				return 0
			}
			c = g.fma(dk, ln2Lo, c)
			return g.fma(dk, ln2Hi, c)
		}
		// R's uses are past a branch, so it is rounded on its own.
		R := float64(hfsq * g.fma(-0.66666666666666666, f, 1.0))
		if k == 0 {
			return f - R
		}
		return g.fma(dk, ln2Hi, -((R - g.fma(dk, ln2Lo, c)) - f))
	}
	s := f / (2.0 + f)
	z := float64(s * s)
	z2 := float64(z * z)
	r2 := g.fma(z, lp3, lp2)
	z4 := float64(z2 * z2)
	r3 := g.fma(z, lp5, lp4)
	z6 := float64(z4 * z2)
	r4 := g.fma(z, lp7, lp6)
	R := g.fma(z6, r4, g.fma(z4, r3, g.fma(z, lp1, float64(z2*r2))))
	if k == 0 {
		return f - g.fma(-s, hfsq+R, hfsq)
	}
	return g.fma(dk, ln2Hi, -((hfsq - g.fma(s, hfsq+R, g.fma(dk, ln2Lo, c))) - f))
}

// log10 is glibc's __ieee754_log10 over log.
func (g glibcOuter) log10(x float64, log func(float64) float64) float64 {
	const (
		two54     float64 = 1.80143985094819840000e+16
		ivln10    float64 = 4.34294481903251816668e-01
		log10_2hi float64 = 3.01029995663611771306e-01
		log10_2lo float64 = 3.69423907715893078616e-13
	)
	hx := int64(math.Float64bits(x))
	k := int64(0)
	if hx < 0x0010000000000000 {
		if hx&0x7fffffffffffffff == 0 {
			return math.Inf(-1)
		}
		if hx < 0 {
			return math.NaN()
		}
		k -= 54
		x *= two54
		hx = int64(math.Float64bits(x))
	}
	if hx >= 0x7ff0000000000000 {
		return x + x
	}
	k += (hx >> 52) - 1023
	i := int64(uint64(k) >> 63)
	hx = (hx & 0x000fffffffffffff) | ((0x3ff - i) << 52)
	y := float64(k + i)
	x = math.Float64frombits(uint64(hx))
	z := g.fma(y, log10_2lo, float64(ivln10*log(x)))
	return g.fma(y, log10_2hi, z)
}

// glibcOuter is how glibc's own double functions are built on the server's architecture: with
// floating-point contraction on aarch64, without on x86_64, whose FMA builds cover only sin, cos,
// tan, the inverse trigonometry, exp, log and pow.
type glibcOuter struct{ contract bool }

// fma is a*b+c as the build computes it: fused, or the product rounded first.
func (g glibcOuter) fma(a, b, c float64) float64 {
	if g.contract {
		return math.FMA(a, b, c)
	}
	return float64(a*b) + c
}

// glibcLibm is glibc 2.31's libm as stackql's Postgres image calls it: exp, log and pow from the
// Arm optimized routines it shares with musl, tan and the inverse trigonometry correctly rounded,
// sin and cos from its FMA build on either architecture, and the rest its own, built as outer says.
type glibcLibm struct {
	muslLibm
	outer glibcOuter
}

func (glibcLibm) Sin(x float64) float64     { return glibcSin(x) }
func (glibcLibm) Cos(x float64) float64     { return glibcCos(x) }
func (l glibcLibm) Cbrt(x float64) float64  { return l.outer.cbrt(x) }
func (l glibcLibm) Log10(x float64) float64 { return l.outer.log10(x, l.Log) }

func (l glibcLibm) Sinh(x float64) float64 {
	jx := highHalf(x)
	ix := jx & 0x7fffffff
	if ix >= 0x7ff00000 {
		return x + x
	}
	h := 0.5
	if jx>>31 != 0 {
		h = -h
	}
	ax := math.Abs(x)
	if ix < 0x40360000 {
		if ix < 0x3e300000 {
			return x
		}
		t := l.outer.expm1(ax)
		if ix < 0x3ff00000 {
			return float64(h * l.outer.fma(2.0, t, -(float64(t*t)/(t+1.0))))
		}
		return float64(h * (t + t/(t+1.0)))
	}
	if ix < 0x40862e42 {
		return float64(h * l.Exp(ax))
	}
	lx := lowHalf(x)
	if ix < 0x408633ce || (ix == 0x408633ce && lx <= 0x8fb9f87d) {
		w := l.Exp(float64(0.5 * ax))
		return float64(float64(h*w) * w)
	}
	return float64(x * 1.0e307)
}

func (l glibcLibm) Cosh(x float64) float64 {
	ix := highHalf(x) & 0x7fffffff
	ax := math.Abs(x)
	if ix < 0x40360000 {
		if ix < 0x3fd62e43 {
			if ix < 0x3c800000 {
				return 1.0
			}
			t := l.outer.expm1(ax)
			w := 1.0 + t
			return 1.0 + float64(t*t)/(w+w)
		}
		t := l.Exp(ax)
		return l.outer.fma(0.5, t, 0.5/t)
	}
	if ix < 0x40862e42 {
		return float64(0.5 * l.Exp(ax))
	}
	lx := lowHalf(x)
	if ix < 0x408633ce || (ix == 0x408633ce && lx <= 0x8fb9f87d) {
		w := l.Exp(float64(0.5 * ax))
		return float64(float64(0.5*w) * w)
	}
	if ix >= 0x7ff00000 {
		return float64(x * x)
	}
	return math.Inf(1)
}

func (l glibcLibm) Tanh(x float64) float64 {
	jx := highHalf(x)
	ix := jx & 0x7fffffff
	if ix >= 0x7ff00000 {
		if jx>>31 == 0 {
			return 1.0/x + 1.0
		}
		return 1.0/x - 1.0
	}
	var z float64
	if ix < 0x40360000 {
		if ix|lowHalf(x) == 0 {
			return x
		}
		if ix < 0x3c800000 {
			return float64(x * (1.0 + x))
		}
		if ix >= 0x3ff00000 {
			t := l.outer.expm1(float64(2.0 * math.Abs(x)))
			z = 1.0 - 2.0/(t+2.0)
		} else {
			t := l.outer.expm1(float64(-2.0 * math.Abs(x)))
			z = -t / (t + 2.0)
		}
	} else {
		z = 1.0 - 1e-300
	}
	if jx>>31 == 0 {
		return z
	}
	return -z
}

func (l glibcLibm) Asinh(x float64) float64 {
	const ln2 float64 = 6.93147180559945286227e-01
	ix := highHalf(x) & 0x7fffffff
	if ix < 0x3e300000 {
		return x
	}
	var w float64
	xa := math.Abs(x)
	switch {
	case ix > 0x41b00000:
		if ix >= 0x7ff00000 {
			return x + x
		}
		w = l.Log(xa) + ln2
	case ix > 0x40000000:
		w = l.Log(l.outer.fma(2.0, xa, 1.0/(math.Sqrt(l.outer.fma(xa, xa, 1.0))+xa)))
	default:
		t := float64(xa * xa)
		w = l.outer.log1p(xa + t/(1.0+math.Sqrt(1.0+t)))
	}
	return math.Copysign(w, x)
}

func (l glibcLibm) Acosh(x float64) float64 {
	const ln2 float64 = 6.93147180559945286227e-01
	hx := int64(math.Float64bits(x))
	switch {
	case hx > 0x4000000000000000:
		if hx >= 0x41b0000000000000 {
			if hx >= 0x7ff0000000000000 {
				return x + x
			}
			return l.Log(x) + ln2
		}
		return l.Log(l.outer.fma(2.0, x, -(1.0 / (x + math.Sqrt(l.outer.fma(x, x, -1.0))))))
	case hx > 0x3ff0000000000000:
		t := x - 1.0
		return l.outer.log1p(t + math.Sqrt(l.outer.fma(2.0, t, float64(t*t))))
	case hx == 0x3ff0000000000000:
		return 0
	}
	return math.NaN()
}

func (l glibcLibm) Atanh(x float64) float64 {
	xa := math.Abs(x)
	var t float64
	switch {
	case xa < 0.5:
		if xa < 0x1.0p-28 {
			return x
		}
		t = xa + xa
		t = float64(0.5 * l.outer.log1p(t+float64(t*xa)/(1.0-xa)))
	case xa < 1.0:
		t = float64(0.5 * l.outer.log1p((xa+xa)/(1.0-xa)))
	case xa > 1.0 || math.IsNaN(xa):
		return math.NaN()
	default:
		return x / 0.0
	}
	return math.Copysign(t, x)
}
