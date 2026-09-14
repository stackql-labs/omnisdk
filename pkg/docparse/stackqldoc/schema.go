package stackqldoc

import (
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
	props map[string]*materialised
	names []string
	items *materialised
	wire  string
}

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
	if node == nil || depth > maxSchemaDepth {
		return nil
	}
	// The annotation belongs to the property; the shape it references is shared by every property of
	// that type, so the wire name is read BEFORE following the reference.
	out := &materialised{wire: xmlName(node)}
	resolved := resolve(node, components)
	if resolved == nil {
		return out
	}
	if items := field(resolved, "items"); items != nil {
		out.items = materialise(items, components, depth+1)
	}
	props := field(resolved, "properties")
	if props == nil {
		return out
	}
	out.props = map[string]*materialised{}
	for i := 0; i+1 < len(props.Content); i += 2 {
		name := props.Content[i].Value
		if child := materialise(props.Content[i+1], components, depth+1); child != nil {
			out.props[name] = child
			out.names = append(out.names, name)
		}
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
