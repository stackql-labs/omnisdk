//go:build linux

package sqlfn

// On Linux, modernc.org/libc runs musl's libm, with fused multiply-adds where musl builds for them.
var libm libmFuncs = muslLibm{fma: muslFastFMA}
