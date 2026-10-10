package sqlfn

// libmFuncs is the C math library SQLite's math functions call. Results differ from one libm to
// another in the last bits, so the SQLite catalogue uses the one stackql's embedded SQLite uses on
// the same platform: modernc.org/libc runs musl's libm on Linux (libm_musl.go) and Go's math package
// everywhere else (libm_go.go).
type libmFuncs interface {
	Acos(float64) float64
	Asin(float64) float64
	Atan(float64) float64
	Atan2(y, x float64) float64
	Cos(float64) float64
	Sin(float64) float64
	Tan(float64) float64
	Cosh(float64) float64
	Sinh(float64) float64
	Tanh(float64) float64
	Acosh(float64) float64
	Asinh(float64) float64
	Atanh(float64) float64
	Exp(float64) float64
	Log(float64) float64
	Log2(float64) float64
	Log10(float64) float64
	Pow(x, y float64) float64
}
