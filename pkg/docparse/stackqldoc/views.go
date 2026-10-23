package stackqldoc

import (
	"fmt"
	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
	"regexp"
	"strings"
)

type viewSpec struct {
	SQL       string    `yaml:"ddl"`
	Condition string    `yaml:"predicate"`
	Next      *viewSpec `yaml:"fallback"`
}

func (v *viewSpec) DDL() string       { return v.SQL }
func (v *viewSpec) Predicate() string { return v.Condition }
func (v *viewSpec) Fallback() (aot.View, bool) {
	if v.Next == nil {
		return nil, false
	}
	return v.Next, true
}

func (c *catalog) View(service, name string) (aot.View, bool, error) {
	doc, err := c.doc(service)
	if err != nil {
		return nil, false, err
	}
	parsed, ok := doc.(*document)
	if !ok {
		return nil, false, nil
	}
	resource, ok := parsed.Components.Resources[name]
	if !ok {
		return nil, false, fmt.Errorf("stackqldoc: no resource %q", name)
	}
	view := resource.Config.Views["select"]
	if view == nil {
		return nil, false, nil
	}
	return view, true, nil
}

var requiredViewParams = regexp.MustCompile(`requiredParams\s*==\s*\[([^\]]*)\]`)

func (v *viewSpec) requiredParams() []string {
	match := requiredViewParams.FindStringSubmatch(v.Condition)
	if len(match) < 2 {
		return nil
	}
	var names []string
	for _, raw := range strings.Split(match[1], ",") {
		if name := strings.Trim(strings.TrimSpace(raw), "\"'"); name != "" {
			names = append(names, name)
		}
	}
	return names
}
