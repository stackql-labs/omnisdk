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
	readOnly   bool
	name       string
	stringOnly bool
	props      map[string]*materialised
	names      []string
	items      *materialised
	wire       string
	typ        string
	fmt        string
	required   []string
}

func (m *materialised) RequiredProperties() []string { return slices.Clone(m.required) }

func (m *materialised) Type() string   { return m.typ }
func (m *materialised) Format() string { return m.fmt }

func (m *materialised) Property(name string) (aot.Schema, bool) {
	p, ok := m.props[name]
	if !ok {
		return nil, false
	}
	return p, true
}

func (m *materialised) Properties() []string { return m.names }

func (m *materialised) Items() (aot.Schema, bool) {
	if m.items == nil {
		return nil, false
	}
	return m.items, true
}

func (m *materialised) WireName() string { return m.wire }

func (m *materialised) StringOnly() bool { return m.stringOnly }

// maxSchemaDepth bounds the copy. A response shape is an envelope, a row type and its scalar
// properties; anything deeper is a recursive definition, and following it would copy the document.
const maxSchemaDepth = 4

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
	return materialiseActive(node, components, depth, map[*yaml.Node]bool{})
}

func materialiseActive(node *yaml.Node, components map[string]*yaml.Node, depth int, active map[*yaml.Node]bool) *materialised {
	if node == nil || depth > maxSchemaDepth {
		return nil
	}
	// The annotation belongs to the property; the shape it references is shared by every property of
	// that type, so the wire name is read BEFORE following the reference.
	out := &materialised{wire: xmlName(node)}
	if ref := field(node, "$ref"); ref != nil {
		out.name = lastSegment(ref.Value)
	}
	resolved := resolve(node, components)
	if resolved == nil {
		return out
	}
	if active[resolved] {
		return out
	}
	active[resolved] = true
	defer delete(active, resolved)
	if annotation := field(resolved, "x-stackQL-stringOnly"); annotation != nil {
		out.stringOnly = annotation.Value == "true"
	}
	// Compositions declare fields at the same object depth. Copy each branch's
	// declared fields, with local properties taking precedence over inherited ones.
	for _, composition := range []string{"allOf", "anyOf", "oneOf"} {
		if branches := field(resolved, composition); branches != nil {
			types := map[string]bool{}
			for _, branch := range branches.Content {
				base := materialiseActive(branch, components, depth, active)
				if base == nil {
					continue
				}
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
		out.items = materialiseActive(items, components, depth+1, active)
	}
	props := field(resolved, "properties")
	if props == nil && len(out.props) == 0 {
		return out
	}
	if out.props == nil {
		out.props = map[string]*materialised{}
	}
	for i := 0; props != nil && i+1 < len(props.Content); i += 2 {
		name := props.Content[i].Value
		if child := materialiseActive(props.Content[i+1], components, depth+1, active); child != nil {
			out.props[name] = child
		}
	}
	for name := range out.props {
		out.names = append(out.names, name)
	}
	sort.Strings(out.names)
	return out
}

// resolve follows a $ref to the schema it names. A document states its row type by reference, so
// without this every property lookup stops at the reference itself.
func resolve(n *yaml.Node, components map[string]*yaml.Node) *yaml.Node {
	for range 8 { // a chain deeper than this is a cycle, not a document
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
	return n
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

func (m *materialised) ReadOnly() bool { return m.readOnly }
