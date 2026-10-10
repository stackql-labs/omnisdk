package sqlfn

import "math"

// The Postgres set-returning functions over integers, numerics and arrays (int.c, int8.c,
// numeric.c, arrayfuncs.c). Their rows are materialized, so a series is held in memory whole.

// pgSeries is generate_series over integers: it stops on overflow rather than wrapping.
func pgSeries(start, stop, step int64, lo, hi int64, wrap func(int64) any) (any, error) {
	if step == 0 {
		return nil, pgErrorf("step size cannot equal zero")
	}
	rows := pgRows{}
	for cur := start; step > 0 && cur <= stop || step < 0 && cur >= stop; {
		rows = append(rows, []any{wrap(cur)})
		next := cur + step
		if next < lo || next > hi || (step > 0) != (next > cur) {
			break
		}
		cur = next
	}
	return rows, nil
}

func pgSeriesNumeric(a []any) (any, error) {
	check := func(n pgNumeric, what string) error {
		switch {
		case n.isNaN():
			return pgErrorf("%s cannot be NaN", what)
		case n.isInf():
			return pgErrorf("%s cannot be infinity", what)
		}
		return nil
	}
	start, stop := num(a[0]), num(a[1])
	if err := check(start, "start value"); err != nil {
		return nil, err
	}
	if err := check(stop, "stop value"); err != nil {
		return nil, err
	}
	step := int64ToNumeric(1)
	if len(a) == 3 {
		step = num(a[2])
		if err := check(step, "step size"); err != nil {
			return nil, err
		}
		if step.signum() == 0 {
			return nil, pgErrorf("step size cannot equal zero")
		}
	}
	rows := pgRows{}
	cur := start.clone()
	for step.sign == numPos && cmpVar(cur, stop) <= 0 || step.sign == numNeg && cmpVar(cur, stop) >= 0 {
		r, err := makeResult(cur)
		if err != nil {
			return nil, err
		}
		rows = append(rows, []any{r})
		cur = addVar(cur, step)
	}
	return rows, nil
}

// pgArrayValues are an array's elements as values of its element type.
func pgArrayValues(v any) []any {
	switch x := v.(type) {
	case pgTextArr:
		out := make([]any, len(x))
		for i, e := range x {
			if e != nil {
				out[i] = *e
			}
		}
		return out
	case pgInt4Arr:
		out := make([]any, len(x))
		for i, e := range x {
			if e != nil {
				out[i] = pgInt4(*e)
			}
		}
		return out
	}
	return nil
}

func init() {
	wrap4 := func(v int64) any { return pgInt4(v) }
	wrap8 := func(v int64) any { return v }
	registerPg("generate_series(int4,int4)", func(a []any) (any, error) {
		return pgSeries(int64(i4(a[0])), int64(i4(a[1])), 1, math.MinInt32, math.MaxInt32, wrap4)
	})
	registerPg("generate_series(int4,int4,int4)", func(a []any) (any, error) {
		return pgSeries(int64(i4(a[0])), int64(i4(a[1])), int64(i4(a[2])), math.MinInt32, math.MaxInt32, wrap4)
	})
	registerPg("generate_series(int8,int8)", func(a []any) (any, error) {
		return pgSeries(i8(a[0]), i8(a[1]), 1, math.MinInt64, math.MaxInt64, wrap8)
	})
	registerPg("generate_series(int8,int8,int8)", func(a []any) (any, error) {
		return pgSeries(i8(a[0]), i8(a[1]), i8(a[2]), math.MinInt64, math.MaxInt64, wrap8)
	})
	registerPg("generate_series(numeric,numeric)", pgSeriesNumeric)
	registerPg("generate_series(numeric,numeric,numeric)", pgSeriesNumeric)

	subscripts := func(a []any) (any, error) {
		n := len(pgArrayValues(a[0]))
		rows := pgRows{}
		// Every array here is one-dimensional with lower bound 1; any other dimension is empty.
		if n == 0 || i4(a[1]) != 1 {
			return rows, nil
		}
		reverse := len(a) == 3 && a[2].(bool)
		for i := 1; i <= n; i++ {
			v := i
			if reverse {
				v = n + 1 - i
			}
			rows = append(rows, []any{pgInt4(v)})
		}
		return rows, nil
	}
	registerPg("generate_subscripts(anyarray,int4)", subscripts)
	registerPg("generate_subscripts(anyarray,int4,bool)", subscripts)
	registerPg("unnest(anyarray)", func(a []any) (any, error) {
		rows := pgRows{}
		for _, v := range pgArrayValues(a[0]) {
			rows = append(rows, []any{v})
		}
		return rows, nil
	})
}
