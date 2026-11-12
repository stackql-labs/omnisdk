package stackqldoc

import (
	"slices"
	"sort"

	"gopkg.in/yaml.v3"

	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
)

// materialised is a declared schema copied out of the document.
//
// Copied, not referenced: a schema node points into the parsed YAML, and holding one keeps the whole
// service document alive. EC2's is tens of megabytes, so a resolved exchange that referenced its
// schema would carry the document it came from — exactly what resolving an exchange exists to avoid.
// Only what the response actually reaches is kept.
type materialised struct {
	target      *materialised
	description string
	enum        []string
	readOnly    bool
	name        string
	stringOnly  bool
	props       map[string]*materialised
	names       []string
	items       *materialised
	wire        string
	typ         string
	fmt         string
	required    []string
}

func (m *materialised) RequiredProperties() []string {
	if m.required != nil {
		return slices.Clone(m.required)
	}
	return slices.Clone(m.shape().required)
}

func (m *materialised) Type() string   { return m.shape().typ }
func (m *materialised) Format() string { return m.shape().fmt }

func (m *materialised) Property(name string) (aot.Schema, bool) {
	p, ok := m.shape().props[name]
	if !ok {
		return nil, false
	}
	return p, true
}

func (m *materialised) Properties() []string { return slices.Clone(m.shape().names) }

func (m *materialised) Items() (aot.Schema, bool) {
	if m.shape().items == nil {
		return nil, false
	}
	return m.shape().items, true
}

func (m *materialised) WireName() string {
	if m.wire != "" {
		return m.wire
	}
	return m.shape().wire
}
func (m *materialised) shape() *materialised {
	if m.target != nil {
		return m.target
	}
	return m
}
func (m *materialised) Description() string {
	if m.description != "" {
		return m.description
	}
	return m.shape().description
}
func (m *materialised) Enum() []string { return slices.Clone(m.shape().enum) }

func (m *materialised) StringOnly() bool { return m.shape().stringOnly }

// newSchema copies the schema at node, resolving references against components. It returns nil where
// the document declares none — which a schema-driven transform must treat as a failure rather than
// as an empty shape.
func newSchema(node *yaml.Node, components map[string]*yaml.Node) aot.Schema {
	s := materialise(node, components, 0)
	if s == nil {
		return nil
	}
	return s
}

func materialise(node *yaml.Node, components map[string]*yaml.Node, depth int) *materialised {
	return materialiseActive(node, components, depth, map[*yaml.Node]*materialised{})
}

func materialiseActive(node *yaml.Node, components map[string]*yaml.Node, depth int, memo map[*yaml.Node]*materialised) *materialised {
	if node == nil {
		return nil
	}
	resolved := resolve(node, components)
	if resolved == nil {
		return &materialised{wire: xmlName(node)}
	}
	alias := func(out *materialised) *materialised {
		if node == resolved {
			return out
		}
		alias := &materialised{target: out, wire: xmlName(node), name: lastSegment(field(node, "$ref").Value)}
		if d := field(node, "description"); d != nil {
			alias.description = d.Value
		}
		return alias
	}
	if existing := memo[resolved]; existing != nil {
		return alias(existing)
	}
	out := &materialised{wire: xmlName(resolved), props: map[string]*materialised{}}
	memo[resolved] = out
	if ref := field(node, "$ref"); ref != nil {
		out.name = lastSegment(ref.Value)
	}
	if d := field(resolved, "description"); d != nil {
		out.description = d.Value
	}
	if e := field(resolved, "enum"); e != nil {
		for _, value := range e.Content {
			out.enum = append(out.enum, value.Value)
		}
	}
	if annotation := field(resolved, "x-stackQL-stringOnly"); annotation != nil {
		out.stringOnly = annotation.Value == "true"
	}
	// Compositions declare fields at the same object depth. Copy each branch's
	// declared fields, with local properties taking precedence over inherited ones.
	for _, composition := range []string{"allOf", "anyOf", "oneOf"} {
		if branches := field(resolved, composition); branches != nil {
			types := map[string]bool{}
			for _, branch := range branches.Content {
				base := materialiseActive(branch, components, depth, memo)
				if base == nil {
					continue
				}
				base = base.shape()
				if out.props == nil {
					out.props = map[string]*materialised{}
				}
				for name, property := range base.props {
					out.props[name] = property
				}
				if base.typ != "" {
					types[base.typ] = true
					out.typ = base.typ
				}
				if base.fmt != "" {
					out.fmt = base.fmt
				}
				if base.items != nil {
					out.items = base.items
				}
				if out.wire == "" {
					out.wire = base.wire
				}
				out.stringOnly = out.stringOnly || base.stringOnly
				if composition == "allOf" {
					out.required = append(out.required, base.required...)
				}
			}
			// Alternatives are a union of accepted types, not the last branch's type.
			if composition != "allOf" && len(types) > 1 {
				out.typ, out.fmt, out.items = "", "", nil
			}
		}
	}
	if readOnly := field(resolved, "readOnly"); readOnly != nil {
		out.readOnly = readOnly.Value == "true"
	}
	if t := field(resolved, "type"); t != nil {
		out.typ = t.Value
	}
	if f := field(resolved, "format"); f != nil {
		out.fmt = f.Value
	}
	if required := field(resolved, "required"); required != nil {
		for _, name := range required.Content {
			out.required = append(out.required, name.Value)
		}
	}
	slices.Sort(out.required)
	out.required = slices.Compact(out.required)
	if items := field(resolved, "items"); items != nil {
		out.items = materialiseActive(items, components, depth+1, memo)
	}
	if out.typ == "" && out.items != nil {
		out.typ = "array"
	}
	props := field(resolved, "properties")
	if out.typ == "" && (props != nil || len(out.props) > 0) {
		out.typ = "object"
	}
	if props == nil && len(out.props) == 0 {
		return alias(out)
	}
	if out.props == nil {
		out.props = map[string]*materialised{}
	}
	for i := 0; props != nil && i+1 < len(props.Content); i += 2 {
		name := props.Content[i].Value
		if child := materialiseActive(props.Content[i+1], components, depth+1, memo); child != nil {
			out.props[name] = child
		}
	}
	for name := range out.props {
		out.names = append(out.names, name)
	}
	sort.Strings(out.names)
	return alias(out)
}

// resolve follows a $ref to the schema it names. A document states its row type by reference, so
// without this every property lookup stops at the reference itself.
func resolve(n *yaml.Node, components map[string]*yaml.Node) *yaml.Node {
	seen := map[*yaml.Node]bool{}
	for n != nil {
		if seen[n] {
			return nil
		}
		seen[n] = true
		ref := field(n, "$ref")
		if ref == nil || ref.Value == "" {
			return n
		}
		target, ok := components[lastSegment(ref.Value)]
		if !ok {
			return nil
		}
		n = target
	}
	return nil
}

func xmlName(n *yaml.Node) string {
	x := field(n, "xml")
	if x == nil {
		return ""
	}
	name := field(x, "name")
	if name == nil {
		return ""
	}
	return name.Value
}

// field reads a mapping's value by key.
func field(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

func (m *materialised) SchemaName() string { return m.name }

func (m *materialised) ReadOnly() bool { return m.shape().readOnly }
