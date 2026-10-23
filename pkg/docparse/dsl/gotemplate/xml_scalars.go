package gotemplate

import (
	"math"
	"strconv"
	"strings"
)

// xmlScalars applies mxj's number-before-boolean convention while preserving
// the SDK's existing guarantee that large integral identifiers remain exact.
func xmlScalars(value any) any {
	switch v := value.(type) {
	case map[string]any:
		for name, child := range v {
			v[name] = xmlScalars(child)
		}
	case []any:
		for i, child := range v {
			v[i] = xmlScalars(child)
		}
	case string:
		if number, err := strconv.ParseFloat(v, 64); err == nil && !math.IsNaN(number) && !math.IsInf(number, 0) {
			if !strings.ContainsAny(v, ".eE") && math.Abs(number) >= 1<<53 {
				return v
			}
			return number
		}
		if len(v) > 0 && strings.ContainsAny(v[:1], "tTfF") {
			if boolean, err := strconv.ParseBool(v); err == nil {
				return boolean
			}
		}
	}
	return value
}
