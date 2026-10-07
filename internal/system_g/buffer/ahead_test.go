package buffer

import (
	"context"
	"testing"
)

func TestWithAhead(t *testing.T) {
	bg := context.Background()
	if RowsAhead(bg) != DefaultRowsAhead || PagesAhead(bg) != DefaultPagesAhead {
		t.Errorf("unset = %d/%d, want the defaults", RowsAhead(bg), PagesAhead(bg))
	}
	for _, c := range []struct{ in, rows, pages int }{
		{0, DefaultRowsAhead, DefaultPagesAhead}, // zero is the default
		{-1, 0, 0},                               // negative is unbounded
		{7, 7, 7},
	} {
		ctx := WithAhead(bg, c.in, c.in)
		if RowsAhead(ctx) != c.rows || PagesAhead(ctx) != c.pages {
			t.Errorf("WithAhead(%d) = %d/%d, want %d/%d", c.in, RowsAhead(ctx), PagesAhead(ctx), c.rows, c.pages)
		}
	}
}
