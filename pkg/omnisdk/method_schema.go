package omnisdk

import (
	"fmt"
	"slices"
	"strings"

	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
)

// FieldSchema is a read-only schema graph. Recursive references may revisit a node;
// use DescribeMethodFields for bounded serialization rather than unfolding it blindly.
type FieldSchema interface {
	Name() string
	WireName() string
	Type() string
	Format() string
	Required() bool
	Description() string
	Enum() []string
	Properties() []FieldSchema
	Items() (FieldSchema, bool)
}

// SchemaMethodSignature is an additive capability; existing MethodSignature implementations
// remain compatible. BodySchema describes inputs; RowSchema selects transformed response rows.
type SchemaMethodSignature interface {
	MethodSignature
	BodySchema() (FieldSchema, bool)
	RowSchema() (FieldSchema, bool)
}

type fieldSchema struct {
	schema     aot.Schema
	name, wire string
	required   bool
}

func (f fieldSchema) Name() string { return f.name }
func (f fieldSchema) WireName() string {
	if n := f.schema.WireName(); n != "" {
		return n
	}
	if f.wire != "" {
		return f.wire
	}
	return f.name
}
func (f fieldSchema) Type() string {
	if s, ok := f.schema.(aot.TypedSchema); ok {
		return s.Type()
	}
	return ""
}
func (f fieldSchema) Format() string {
	if s, ok := f.schema.(aot.TypedSchema); ok {
		return s.Format()
	}
	return ""
}
func (f fieldSchema) Required() bool { return f.required }
func (f fieldSchema) Description() string {
	if s, ok := f.schema.(aot.DescribedSchema); ok {
		return s.Description()
	}
	return ""
}
func (f fieldSchema) Enum() []string {
	if s, ok := f.schema.(aot.DescribedSchema); ok {
		return slices.Clone(s.Enum())
	}
	return nil
}
func (f fieldSchema) Properties() []FieldSchema {
	var required []string
	if s, ok := f.schema.(aot.RequiredSchema); ok {
		required = s.RequiredProperties()
	}
	var out []FieldSchema
	for _, name := range f.schema.Properties() {
		if p, ok := f.schema.Property(name); ok {
			out = append(out, fieldSchema{schema: p, name: name, required: slices.Contains(required, name)})
		}
	}
	return out
}
func (f fieldSchema) Items() (FieldSchema, bool) {
	s, ok := f.schema.Items()
	if !ok {
		return nil, false
	}
	return fieldSchema{schema: s}, true
}
func (s docSignature) BodySchema() (FieldSchema, bool) {
	body := s.RequestBodySchema()
	if body == nil {
		return nil, false
	}
	return fieldSchema{schema: body}, true
}
func (s docSignature) RowSchema() (FieldSchema, bool) {
	row := s.row()
	if row == nil || recursiveEnvelope(row) {
		return nil, false
	}
	return fieldSchema{schema: row}, true
}

// MethodSchemaOptions bounds discovery. Depth defaults to 1 (maximum 32).
// Path is a dot-separated input/output subtree, e.g. data__interfaces.accessConfigs.
// Array items are traversed implicitly. ParamType accepts input, output, input_required,
// or input_optional. Descriptions are opt-in. Global query parameters are omitted by default.
type MethodSchemaOptions struct {
	Depth         int    `json:"depth,omitempty"`
	Path          string `json:"path,omitempty"`
	Descriptions  bool   `json:"descriptions,omitempty"`
	ParamType     string `json:"param_type,omitempty"`
	IncludeGlobal bool   `json:"include_global,omitempty"`
}

// FieldDescription is a bounded serialization DTO. Children counts object properties,
// including array-item properties. Truncated indicates that further drill-down is available.
type FieldDescription struct {
	Name        string             `json:"name,omitempty"`
	WireName    string             `json:"wire_name,omitempty"`
	ParamType   string             `json:"param_type,omitempty"`
	Type        string             `json:"type,omitempty"`
	Format      string             `json:"format,omitempty"`
	Required    bool               `json:"required"`
	Description string             `json:"description,omitempty"`
	Enum        []string           `json:"enum,omitempty"`
	Children    int                `json:"children"`
	Truncated   bool               `json:"truncated,omitempty"`
	Properties  []FieldDescription `json:"properties,omitempty"`
	Items       *FieldDescription  `json:"items,omitempty"`
}

// DescribeMethodFields returns a bounded method contract without provider calls.
// Unknown types remain unspecified; they are never invented as string.
func DescribeMethodFields(dir, address, method string, options MethodSchemaOptions) ([]FieldDescription, error) {
	if options.Depth == 0 {
		options.Depth = 1
	}
	if options.Depth < 1 || options.Depth > 32 {
		return nil, fmt.Errorf("schema depth must be between 1 and 32")
	}
	switch options.ParamType {
	case "", "input", "output", "input_required", "input_optional":
	default:
		return nil, fmt.Errorf("unknown param_type %q", options.ParamType)
	}
	signature, err := DescribeMethod(dir, address, method)
	if err != nil {
		return nil, err
	}
	s, ok := signature.(docSignature)
	if !ok {
		return nil, fmt.Errorf("method has no document schema")
	}
	type entry struct {
		field FieldSchema
		kind  string
	}
	var entries []entry
	body, _ := s.BodySchema()
	for _, p := range s.Params() {
		if p.In() == aot.InBody {
			continue
		}
		// OpenAPI's provider-wide Google query controls are protocol metadata, not body inputs.
		if global, ok := p.(interface{ Global() bool }); !options.IncludeGlobal && ok && global.Global() {
			continue
		}
		typ, format := "", ""
		if v, ok := p.(interface{ Type() string }); ok {
			typ = v.Type()
		}
		if v, ok := p.(interface{ Format() string }); ok {
			format = v.Format()
		}
		meta := &discoverySchema{typ: typ, format: format}
		if v, ok := p.(aot.DescribedSchema); ok {
			meta.description, meta.enum = v.Description(), v.Enum()
		}
		kind := "input_optional"
		if p.Required() {
			kind = "input_required"
		}
		entries = append(entries, entry{fieldSchema{schema: meta, name: s.surface(p.Name()), wire: p.Name(), required: p.Required()}, kind})
	}
	if body != nil {
		for _, f := range body.Properties() {
			required := false
			for _, p := range s.Params() {
				if p.In() == aot.InBody && p.Name() == f.Name() {
					required = p.Required()
					break
				}
			}
			kind := "input_optional"
			if required {
				kind = "input_required"
			}
			sf := f.(fieldSchema)
			sf.name = "data__" + s.surface(f.Name())
			sf.wire = f.WireName()
			sf.required = required
			entries = append(entries, entry{sf, kind})
		}
	}
	wantOutputs := options.ParamType == "" || options.ParamType == "output"
	if options.Path != "" {
		root := strings.Split(options.Path, ".")[0]
		for _, e := range entries {
			if root == e.field.Name() || (strings.HasPrefix(e.field.Name(), "data__") && root == "data__"+e.field.WireName()) {
				wantOutputs = false
				break
			}
		}
	}
	if wantOutputs {
		if recursiveEnvelope(s.row()) {
			return nil, fmt.Errorf("method %s has a self-referencing response envelope; provider document must declare a row schema", method)
		}
		if row, ok := s.RowSchema(); ok {
			for _, f := range row.Properties() {
				sf := f.(fieldSchema)
				sf.name = s.responseName(f.Name())
				sf.wire = f.WireName()
				entries = append(entries, entry{sf, "output"})
			}
		}
	}
	budget := 4096
	var out []FieldDescription
	for _, e := range entries {
		if options.ParamType != "" && e.kind != options.ParamType && !(options.ParamType == "input" && strings.HasPrefix(e.kind, "input_")) {
			continue
		}
		f := e.field
		if options.Path != "" {
			parts := strings.Split(options.Path, ".")
			if parts[0] != f.Name() && !(strings.HasPrefix(f.Name(), "data__") && parts[0] == "data__"+f.WireName()) {
				continue
			}
			for _, name := range parts[1:] {
				for hops := 0; ; hops++ {
					if hops >= 32 {
						return nil, fmt.Errorf("schema path %q traverses recursive array items", options.Path)
					}
					item, ok := f.Items()
					if !ok {
						break
					}
					f = item
				}
				var next FieldSchema
				for _, child := range f.Properties() {
					if child.Name() == name || child.WireName() == name {
						next = child
						break
					}
				}
				if next == nil {
					return nil, fmt.Errorf("schema path %q does not exist", options.Path)
				}
				f = next
			}
		}
		view, err := describeField(f, e.kind, options.Depth, options.Descriptions, &budget)
		if err != nil {
			return nil, err
		}
		out = append(out, view)
	}
	if options.Path != "" && len(out) == 0 {
		return nil, fmt.Errorf("schema path %q does not exist", options.Path)
	}
	return out, nil
}

func describeField(f FieldSchema, kind string, depth int, descriptions bool, budget *int) (FieldDescription, error) {
	*budget--
	if *budget < 0 {
		return FieldDescription{}, fmt.Errorf("schema view exceeds 4096 nodes; narrow path or depth")
	}
	out := FieldDescription{Name: f.Name(), ParamType: kind, Type: f.Type(), Format: f.Format(), Required: f.Required(), Enum: f.Enum()}
	if wire := f.WireName(); wire != "" && wire != f.Name() {
		out.WireName = wire
	}
	if descriptions {
		out.Description = f.Description()
	}
	children := f.Properties()
	out.Children = len(children)
	if depth > 1 {
		for _, child := range children {
			view, err := describeField(child, "", depth-1, descriptions, budget)
			if err != nil {
				return out, err
			}
			out.Properties = append(out.Properties, view)
		}
	} else {
		out.Truncated = len(children) > 0
	}
	if item, ok := f.Items(); ok {
		if depth > 1 {
			view, err := describeField(item, "", depth-1, descriptions, budget)
			if err != nil {
				return out, err
			}
			out.Items = &view
			out.Children = max(out.Children, view.Children)
		} else {
			// Keep the array element type/count visible without recursively unfolding it.
			count := len(item.Properties())
			out.Items = &FieldDescription{Type: item.Type(), Children: count, Truncated: count > 0}
			out.Children = max(out.Children, count)
			out.Truncated = true
		}
	}
	return out, nil
}

// discoverySchema adapts scalar parameter metadata to the same read-only field contract.
type discoverySchema struct {
	typ, format, description string
	enum                     []string
}

func (s *discoverySchema) Property(string) (aot.Schema, bool) { return nil, false }
func (s *discoverySchema) Properties() []string               { return nil }
func (s *discoverySchema) Items() (aot.Schema, bool)          { return nil, false }
func (s *discoverySchema) WireName() string                   { return "" }
func (s *discoverySchema) Type() string                       { return s.typ }
func (s *discoverySchema) Format() string                     { return s.format }
func (s *discoverySchema) Description() string                { return s.description }
func (s *discoverySchema) Enum() []string                     { return slices.Clone(s.enum) }

func recursiveEnvelope(schema aot.Schema) bool {
	if schema == nil || !slices.Equal(schema.Properties(), []string{"line_items"}) {
		return false
	}
	envelope, ok := schema.(aot.NamedSchema)
	if !ok || envelope.SchemaName() == "" {
		return false
	}
	field, ok := schema.Property("line_items")
	if !ok {
		return false
	}
	item, ok := field.Items()
	if !ok {
		return false
	}
	named, ok := item.(aot.NamedSchema)
	return ok && named.SchemaName() == envelope.SchemaName()
}
