package apply_test

import (
	"testing"

	"github.com/stackql-labs/omnisdk/internal/apply"
)

// Satisfaction compares numbers as written, so a value beyond float64's exact range is not reported
// as satisfied when it differs in the last digit; a field to unset is satisfied only by its absence.
func TestSatisfied(t *testing.T) {
	for name, c := range map[string]struct {
		mutation, actual string
		want             bool
	}{
		"equal":            {`{"n":10000000000000001}`, `{"n":10000000000000001}`, true},
		"last digit":       {`{"n":10000000000000001}`, `{"n":10000000000000000}`, false},
		"unset and absent": {`{"a":null}`, `{}`, true},
		"unset but there":  {`{"a":null}`, `{"a":1}`, false},
		"nested":           {`{"o":{"x":"y"}}`, `{"o":{"x":"y","z":1}}`, true},
		"missing":          {`{"a":1}`, `{}`, false},
		"unreadable":       {`{"a":1}`, `not json`, false},
	} {
		got, err := apply.Satisfied([]byte(c.mutation), []byte(c.actual))
		if err != nil || got != c.want {
			t.Errorf("%s: got %v, %v; want %v", name, got, err, c.want)
		}
	}
}
