package sqlfn

// SetNow replaces the clock the date functions read, as a Julian day in milliseconds, and returns a
// function restoring it.
func SetNow(f func() int64) (restore func()) {
	prev := sqliteNow
	sqliteNow = f
	return func() { sqliteNow = prev }
}
