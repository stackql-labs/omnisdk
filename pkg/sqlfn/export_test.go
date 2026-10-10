package sqlfn

// Postgres-typed values, for tests that write each argument as stackql's data reaches Postgres.
type (
	PgUnknown = pgUnknown
	PgInt4    = pgInt4
	PgFloat8  = pgFloat8
	PgJSON    = pgJSON
	PgJSONB   = pgJSONB
	PgTextArr = pgTextArr
)

// PgTyped is a Postgres value of type t read from its text, as t's input function reads it.
func PgTyped(t, text string) (any, error) {
	c, err := BuiltinsFor(Postgres)
	if err != nil {
		return nil, err
	}
	f, _ := c.Get("cast")
	return f.Call([]any{PgUnknown(text), PgUnknown(t)})
}

// PgText is a Postgres result's type and text output, as ::text and pg_typeof report them.
func PgText(v any) (string, string, bool) {
	s, ok := pgTextOf(v)
	return pgDisplayName(pgTypeNameOf(v)), s, ok
}

// PgArrayText is a text array's output.
func PgArrayText(a PgTextArr) string { return pgArrayText(a) }
