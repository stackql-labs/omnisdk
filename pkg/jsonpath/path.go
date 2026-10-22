// Package jsonpath selects fields and array elements from decoded JSON documents.
package jsonpath

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Segment is one property or array step; schema readers use Array to follow items.
type Segment interface {
	Name() string
	Array() bool
}

// Selection is a parsed path. Missing values produce no matches.
type Selection interface {
	Values(any) []any
	Segments() []Segment
}
type step struct {
	name       string
	array      bool
	wildcard   bool
	index      int
	slice      bool
	start, end *int
}

func (s step) Name() string { return s.name }
func (s step) Array() bool  { return s.array }

type selection struct{ steps []step }

func (s selection) Segments() []Segment {
	out := make([]Segment, len(s.steps))
	for i, v := range s.steps {
		out[i] = v
	}
	return out
}

// Parse supports property paths, wildcards, array indices and half-open slices.
// Unsupported expressions are rejected rather than interpreted as empty results.
func Parse(path string) (Selection, error) {
	p := strings.TrimSpace(path)
	if strings.HasPrefix(p, "$") {
		p = p[1:]
	}
	var out []step
	for len(p) > 0 {
		if p[0] == '.' {
			p = p[1:]
			if p == "" {
				return nil, fmt.Errorf("jsonpath: trailing dot in %q", path)
			}
		}
		if p[0] != '[' {
			end := strings.IndexAny(p, ".[")
			if end < 0 {
				end = len(p)
			}
			if end == 0 {
				return nil, fmt.Errorf("jsonpath: invalid property in %q", path)
			}
			name := p[:end]
			if name == "*" {
				out = append(out, step{array: true, wildcard: true})
				p = p[end:]
				continue
			}
			if strings.ContainsAny(name, " ()?@*$'") {
				return nil, fmt.Errorf("jsonpath: unsupported property %q", name)
			}
			out = append(out, step{name: name})
			p = p[end:]
			continue
		}
		end := strings.IndexByte(p, ']')
		if end < 0 {
			return nil, fmt.Errorf("jsonpath: unclosed bracket in %q", path)
		}
		part := strings.TrimSpace(p[1:end])
		p = p[end+1:]
		if p != "" && p[0] != '.' && p[0] != '[' {
			return nil, fmt.Errorf("jsonpath: missing separator in %q", path)
		}
		if len(part) >= 2 && ((part[0] == '\'' && part[len(part)-1] == '\'') || (part[0] == '"' && part[len(part)-1] == '"')) {
			name := part[1 : len(part)-1]
			if part[0] == '"' {
				var err error
				name, err = strconv.Unquote(part)
				if err != nil {
					return nil, err
				}
			}
			out = append(out, step{name: name})
			continue
		}
		if part == "*" {
			out = append(out, step{array: true, wildcard: true})
			continue
		}
		if a, b, ok := strings.Cut(part, ":"); ok {
			s := step{array: true, slice: true}
			if a != "" {
				n, err := strconv.Atoi(a)
				if err != nil {
					return nil, fmt.Errorf("jsonpath: invalid slice %q", part)
				}
				s.start = &n
			}
			if b != "" {
				n, err := strconv.Atoi(b)
				if err != nil {
					return nil, fmt.Errorf("jsonpath: invalid slice %q", part)
				}
				s.end = &n
			}
			out = append(out, s)
			continue
		}
		index, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("jsonpath: unsupported selector %q", part)
		}
		out = append(out, step{array: true, index: index})
	}
	return selection{out}, nil
}

func (s selection) Values(doc any) []any {
	values := []any{doc}
	for _, step := range s.steps {
		next := []any{}
		for _, v := range values {
			if !step.array {
				if m, ok := v.(map[string]any); ok {
					if child, ok := m[step.name]; ok {
						next = append(next, child)
					}
				}
				continue
			}
			if m, ok := v.(map[string]any); ok && step.wildcard {
				keys := make([]string, 0, len(m))
				for key := range m {
					keys = append(keys, key)
				}
				sort.Strings(keys)
				for _, key := range keys {
					next = append(next, m[key])
				}
				continue
			}
			items, ok := v.([]any)
			if !ok {
				continue
			}
			if step.wildcard {
				next = append(next, items...)
				continue
			}
			if step.slice {
				lo, hi := 0, len(items)
				if step.start != nil {
					lo = position(*step.start, len(items))
				}
				if step.end != nil {
					hi = position(*step.end, len(items))
				}
				if lo < hi {
					next = append(next, items[lo:hi]...)
				}
				continue
			}
			index := step.index
			if index < 0 {
				index += len(items)
			}
			if index >= 0 && index < len(items) {
				next = append(next, items[index])
			}
		}
		values = next
	}
	return values
}
func position(i, n int) int {
	if i < 0 {
		i += n
	}
	if i < 0 {
		return 0
	}
	if i > n {
		return n
	}
	return i
}
