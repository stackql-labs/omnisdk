package namespace

import (
	"fmt"
	"regexp"
	"strings"
)

// Naming maps the SQL surface to the names declared by a foreign API.
type Naming interface {
	Surface(string) string
	Native(string, []string) (string, error)
}

// NewNaming uses the method's native casing and the provider's alias opt-in.
func NewNaming(native string, aliases bool) Naming { return naming{native, aliases} }

type naming struct {
	native  string
	aliases bool
}

var firstCap = regexp.MustCompile(`(.)([A-Z][a-z]+)`)
var numberCap = regexp.MustCompile(`([a-z])([0-9]+)`)
var endCap = regexp.MustCompile(`([a-z0-9])([A-Z])`)
var pluralCap = regexp.MustCompile(`[A-Z]{2,}s$`)

func snake(s string) string {
	if strings.Contains(s, "-") {
		parts := strings.Split(s, "-")
		for i := range parts {
			parts[i] = snake(parts[i])
		}
		return strings.Join(parts, "_")
	}
	if strings.Contains(s, "_") {
		return s
	}
	if m := pluralCap.FindString(s); m != "" {
		s = s[:len(s)-len(m)] + "_" + strings.ToLower(m)
	}
	s = firstCap.ReplaceAllString(s, "${1}_${2}")
	s = numberCap.ReplaceAllString(s, "${1}_${2}")
	return strings.ToLower(endCap.ReplaceAllString(s, "${1}_${2}"))
}

func (n naming) Surface(s string) string {
	if n.aliases {
		return snake(s)
	}
	return s
}

func (n naming) Native(s string, declared []string) (string, error) {
	for _, key := range declared {
		if key == s {
			return key, nil
		}
	}
	if n.native == "" {
		return s, nil
	}
	match := ""
	for _, key := range declared {
		if snake(key) == s {
			if match != "" {
				return "", fmt.Errorf("name %q is ambiguous between %q and %q", s, match, key)
			}
			match = key
		}
	}
	if match != "" {
		return match, nil
	}
	return s, nil
}
