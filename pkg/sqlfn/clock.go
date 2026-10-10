package sqlfn

import (
	"sync"
	"time"
)

// Clock is where a catalogue's functions read the current time. SQLite reads it once per statement
// and gives every 'now' in that statement the same instant; StatementClock does the same.
type Clock interface {
	Now() time.Time
}

// StatementClock is a clock for one statement: the first read takes the time from source, and every
// later read returns that same instant.
func StatementClock(source func() time.Time) Clock { return &statementClock{source: source} }

type statementClock struct {
	once   sync.Once
	source func() time.Time
	at     time.Time
}

func (c *statementClock) Now() time.Time {
	c.once.Do(func() { c.at = c.source() })
	return c.at
}

// FixedClock is a clock that always reads t.
func FixedClock(t time.Time) Clock { return fixedClock(t) }

type fixedClock time.Time

func (c fixedClock) Now() time.Time { return time.Time(c) }
