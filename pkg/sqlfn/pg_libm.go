package sqlfn

// pgLibmFuncs is the C math library Postgres's float8 functions call.
type pgLibmFuncs interface {
	libmFuncs
	Cbrt(float64) float64
}

// pgLibmFor is the C library stackql's Postgres image (postgres:14.5-bullseye) runs on, glibc 2.31,
// as built for the server's architecture.
func pgLibmFor(a PostgresArch) pgLibmFuncs {
	return glibcLibm{muslLibm: muslLibm{fma: true}, outer: glibcOuter{contract: a == PostgresARM64}}
}

// pgLibm is pgLibmFor either architecture, for the functions whose builds agree: sin, cos, tan,
// the inverse trigonometry, exp, log and pow.
var pgLibm = pgLibmFor(PostgresAMD64)
