// Package schemaxml implements the schema-driven XML transform a provider document can name.
//
// It is the odd one out among the little languages: the document names it and ships NO program,
// because its instructions are the document's own response schema. The schema says what a row looks
// like and, per property, the element name the wire uses — so the transform is the inverse of those
// annotations plus a projection onto the declared properties.
//
// A document naming this and finding no implementation does not fail loudly: its declared row path
// points at a shape only this transform produces, so the query returns nothing and says nothing
// about why. That is the reason for implementing it rather than working around it per address.
package schemaxml

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
	"github.com/stackql-labs/omnisdk/pkg/docparse/dsl"
)

// Type is the identifier a document uses to name this transform.
const Type = "schema_driven_xml_v0.1.0"

// Schema is the document-declared shape this transform is driven by. It is aot's contract, so a
// document format supplies the implementation and nothing here learns how the document was written.
type Schema = aot.Schema

// New returns the evaluator. It holds no state: every call is driven by the context it is given.
func New() dsl.Evaluator { return evaluator{} }

type evaluator struct{}

func (evaluator) Type() string { return Type }

// Eval transforms an XML body into the envelope the document's row path expects.
//
// program is ignored and expected to be empty: this language has no text. Everything it needs comes
// from the context, and a caller that supplies no schema gets an error rather than an empty result
// that would read as "the provider returned nothing".
func (e evaluator) Eval(ctx dsl.Context, program string, in []byte) ([]byte, error) {
	if strings.TrimSpace(program) != "" {
		return nil, fmt.Errorf("schemaxml: %s takes no program; its instructions are the schema", Type)
	}
	schema, ok := ctx.Schema.(Schema)
	if !ok || schema == nil {
		return nil, fmt.Errorf("schemaxml: no response schema; this transform IS the schema applied")
	}
	listProperty := ctx.ListProperty
	if listProperty == "" {
		return nil, fmt.Errorf("schemaxml: no list property; there is nowhere to put the rows")
	}
	rowSchema, err := rowSchemaOf(schema, listProperty)
	if err != nil {
		return nil, err
	}

	// Decoded WITHOUT numeric casting: an identifier that is all digits must survive as the digits
	// the provider sent, not as a float64's idea of them.
	body, err := decode(in)
	if err != nil {
		return nil, err
	}

	payload := unwrapEnvelope(body, ctx.Protocol)
	rows, scalars := extractRows(payload, rowSchema)

	out := map[string]any{listProperty: rows}
	// Scalars beside the list — a pagination token, a request id — pass through: they belong to the
	// response rather than to any row, and dropping them would lose the next page.
	for k, v := range scalars {
		if k != listProperty {
			out[k] = v
		}
	}
	return json.Marshal(out)
}

// rowSchemaOf resolves the per-row shape: the list property's element schema.
func rowSchemaOf(schema Schema, listProperty string) (Schema, error) {
	list, ok := schema.Property(listProperty)
	if !ok {
		return nil, fmt.Errorf("schemaxml: the response schema declares no %q", listProperty)
	}
	row, ok := list.Items()
	if !ok {
		return nil, fmt.Errorf("schemaxml: %q is not a list, so it has no row shape", listProperty)
	}
	return row, nil
}

// unwrapEnvelope strips the protocol's outer wrapper. AWS's query and ec2 protocols wrap every reply
// in an <ActionResponse> element that names the call rather than the data, and a row lives beneath
// it; rest-xml has no such wrapper.
func unwrapEnvelope(body map[string]any, protocol string) map[string]any {
	switch protocol {
	case "rest-xml":
		return body
	}
	if len(body) != 1 {
		return body
	}
	for _, v := range body {
		if inner, ok := v.(map[string]any); ok {
			return inner
		}
	}
	return body
}

// extractRows finds the rows in a payload, in three shapes a provider actually uses.
//
// The order matters: a payload that itself looks like a row is a singleton reply and must not be
// searched for a list, or a row whose fields happen to include a nested object would be mistaken
// for the list itself.
func extractRows(payload map[string]any, row Schema) ([]any, map[string]any) {
	// A reply about one object: the payload IS the row.
	if looksLikeRow(payload, row) {
		return []any{projectRow(payload, row)}, nil
	}
	// A single named wrapper — <vpc>…</vpc> — around one row. A mutating reply carries scalars
	// beside it (a requestId, a return flag), so the wrapper is sought among the object children
	// rather than requiring it to be the only child: demanding that made a create's reply project
	// to no rows at all, and an identity declared against the row shape then had nowhere to read.
	if wrapped, ok := soleRowObject(payload, row); ok {
		return []any{projectRow(wrapped, row)}, scalarsBeside(payload)
	}
	// A list member beside scalar siblings: <vpcSet><item>…</item></vpcSet> plus a token.
	key, items := findListMember(payload, row)
	if key == "" {
		return []any{}, payload
	}
	rows := make([]any, 0, len(items))
	for _, it := range items {
		if m, ok := it.(map[string]any); ok {
			rows = append(rows, projectRow(m, row))
		}
	}
	scalars := map[string]any{}
	for k, v := range payload {
		if k != key {
			if _, isObj := v.(map[string]any); !isObj {
				scalars[k] = v
			}
		}
	}
	return rows, scalars
}

// soleRowObject finds the one object child that carries the row's declared elements. More than one
// candidate is ambiguous — the caller asked about a single object and nothing says which — so it
// reports nothing rather than guessing.
func soleRowObject(payload map[string]any, row Schema) (map[string]any, bool) {
	var found map[string]any
	for _, v := range payload {
		m, isObj := v.(map[string]any)
		if !isObj || !looksLikeRow(m, row) {
			continue
		}
		if found != nil {
			return nil, false
		}
		found = m
	}
	return found, found != nil
}

// scalarsBeside is what the response carries outside the row: a request id, a return flag. They
// belong to the reply rather than to the object, and dropping them loses what the call reported.
func scalarsBeside(payload map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range payload {
		if _, isObj := v.(map[string]any); !isObj {
			out[k] = v
		}
	}
	return out
}

// looksLikeRow reports whether a map carries the row's declared elements. One match is enough: a
// provider omits empty fields, so demanding all of them would reject most real replies.
func looksLikeRow(m map[string]any, row Schema) bool {
	for _, name := range row.Properties() {
		if _, ok := m[wireNameOf(row, name)]; ok {
			return true
		}
	}
	return false
}

// findListMember locates the member holding the rows, preferring one whose items look like the row.
// Keys are considered in sorted order so the same document always yields the same choice.
func findListMember(payload map[string]any, row Schema) (string, []any) {
	keys := make([]string, 0, len(payload))
	for k := range payload {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		items := itemsOf(payload[k])
		if len(items) == 0 {
			continue
		}
		if m, ok := items[0].(map[string]any); ok && looksLikeRow(m, row) {
			return k, items
		}
	}
	return "", nil
}

// itemsOf reads a member as a list. XML has no arrays, so a set is either <x><item>…</item></x> with
// one item decoded as an object, or several decoded as a slice.
func itemsOf(v any) []any {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	inner, ok := m["item"]
	if !ok {
		return nil
	}
	switch t := inner.(type) {
	case []any:
		return t
	default:
		return []any{t}
	}
}

// projectRow maps a decoded element onto the schema's property names, keeping only what the document
// declares. A field the schema does not mention is dropped: the schema is the contract, and passing
// undeclared fields through would make the row shape depend on what the provider happened to send.
func projectRow(m map[string]any, row Schema) map[string]any {
	out := map[string]any{}
	for _, name := range row.Properties() {
		if v, ok := m[wireNameOf(row, name)]; ok {
			out[name] = v
		}
	}
	return out
}

// wireNameOf is the element name a property takes on the wire: what the document annotates, or the
// property name itself where it annotates nothing.
func wireNameOf(row Schema, property string) string {
	p, ok := row.Property(property)
	if !ok {
		return property
	}
	if w := p.WireName(); w != "" {
		return w
	}
	return property
}

// decode reads XML into nested maps, keeping every leaf as the text it was written as.
//
// No type casting anywhere: an identifier that is all digits must survive as those digits. Decoding
// through a numeric type rewrites anything past float64's exact range, and an id is exactly the sort
// of value that lands there.
func decode(in []byte) (map[string]any, error) {
	dec := xml.NewDecoder(strings.NewReader(string(in)))
	var root map[string]any
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("schemaxml: %w", err)
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		v, err := element(dec)
		if err != nil {
			return nil, err
		}
		root = map[string]any{start.Name.Local: v}
		break
	}
	if root == nil {
		return nil, fmt.Errorf("schemaxml: no XML element found")
	}
	return root, nil
}

// element reads one element's content: its children as a map, or its text where it has none. A name
// appearing more than once becomes a slice, which is how XML expresses a list.
func element(dec *xml.Decoder) (any, error) {
	children := map[string]any{}
	var text strings.Builder
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("schemaxml: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			child, err := element(dec)
			if err != nil {
				return nil, err
			}
			name := t.Name.Local
			switch existing := children[name].(type) {
			case nil:
				if _, seen := children[name]; seen {
					children[name] = []any{nil, child}
					continue
				}
				children[name] = child
			case []any:
				children[name] = append(existing, child)
			default:
				children[name] = []any{existing, child}
			}
		case xml.CharData:
			text.Write(t)
		case xml.EndElement:
			if len(children) > 0 {
				return children, nil
			}
			return strings.TrimSpace(text.String()), nil
		}
	}
}
