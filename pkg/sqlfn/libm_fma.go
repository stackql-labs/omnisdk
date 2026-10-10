//go:build linux && (arm64 || ppc64le || riscv64 || loong64 || s390x)

package sqlfn

// muslFastFMA: musl takes its fused multiply-add paths on these targets, as modernc.org/libc's
// transpilation of it does.
const muslFastFMA = true
