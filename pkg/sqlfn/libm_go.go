//go:build !linux

package sqlfn

import "math"

// goLibm is Go's math package, which modernc.org/libc's math functions call off Linux.
type goLibm struct{}

var libm libmFuncs = goLibm{}

func (goLibm) Acos(x float64) float64     { return math.Acos(x) }
func (goLibm) Asin(x float64) float64     { return math.Asin(x) }
func (goLibm) Atan(x float64) float64     { return math.Atan(x) }
func (goLibm) Atan2(y, x float64) float64 { return math.Atan2(y, x) }
func (goLibm) Cos(x float64) float64      { return math.Cos(x) }
func (goLibm) Sin(x float64) float64      { return math.Sin(x) }
func (goLibm) Tan(x float64) float64      { return math.Tan(x) }
func (goLibm) Cosh(x float64) float64     { return math.Cosh(x) }
func (goLibm) Sinh(x float64) float64     { return math.Sinh(x) }
func (goLibm) Tanh(x float64) float64     { return math.Tanh(x) }
func (goLibm) Acosh(x float64) float64    { return math.Acosh(x) }
func (goLibm) Asinh(x float64) float64    { return math.Asinh(x) }
func (goLibm) Atanh(x float64) float64    { return math.Atanh(x) }
func (goLibm) Exp(x float64) float64      { return math.Exp(x) }
func (goLibm) Log(x float64) float64      { return math.Log(x) }
func (goLibm) Log2(x float64) float64     { return math.Log2(x) }
func (goLibm) Log10(x float64) float64    { return math.Log10(x) }
func (goLibm) Pow(x, y float64) float64   { return math.Pow(x, y) }
