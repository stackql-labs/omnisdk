//go:build linux && !(arm64 || ppc64le || riscv64 || loong64 || s390x)

package sqlfn

// muslFastFMA: musl takes its paths without fused multiply-add on these targets.
const muslFastFMA = false
