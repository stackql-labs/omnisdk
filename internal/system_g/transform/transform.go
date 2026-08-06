package transform

import (
	"encoding/json"
	"fmt"

	"github.com/clbanning/mxj/v2"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/record"
	"github.com/stackql-labs/omnisdk/internal/system_g/value"
)

var (
	_ facade.Transform = xmlToAgnostic{}
	_ facade.Transform = jsonToAgnostic{}
	_ facade.Transform = projection{}
)

// jsonToAgnostic decodes the JSON in the page's anonymous payload into the encoding-agnostic
// document tree, carried onward as a docValue — the JSON twin of xmlToAgnostic. The two are
// drop-in replaceable: swap which one you compose to change the wire encoding, nothing else.
type jsonToAgnostic struct{}

// NewJSONToAgnostic builds the JSON→agnostic decode transform.
func NewJSONToAgnostic() facade.Transform {
	return jsonToAgnostic{}
}

func (t jsonToAgnostic) Apply(in facade.Page) (facade.Record, error) {
	b := in.Bytes(facade.AnonymousPayload)
	if len(b) == 0 {
		return nil, nil
	}
	var doc any
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	return record.NewRecord(map[string]facade.Value{
		facade.AnonymousPayload: value.NewDocValue(doc),
	}), nil
}

// xmlToAgnostic decodes the XML in the page's anonymous payload into the encoding-agnostic
// document tree (mxj), carried onward as a docValue. This is the intermediate that later
// transforms and encoders work against, independent of the source wire format.
type xmlToAgnostic struct{}

// NewXMLToAgnostic builds the XML→agnostic transform.
func NewXMLToAgnostic() facade.Transform {
	return xmlToAgnostic{}
}

func (t xmlToAgnostic) Apply(in facade.Page) (facade.Record, error) {
	b := in.Bytes(facade.AnonymousPayload)
	if len(b) == 0 {
		return nil, nil
	}
	m, err := mxj.NewMapXml(b)
	if err != nil {
		return nil, err
	}
	return record.NewRecord(map[string]facade.Value{
		facade.AnonymousPayload: value.NewDocValue(map[string]interface{}(m)),
	}), nil
}

// Column selects one output field: Out is the emitted key, Path is a dot-path into a row of
// the agnostic tree (mxj path syntax).
type Column struct {
	Out  string
	Path string
}

// projection maps the agnostic tree to a list of rows of selected columns. rowPath locates
// the repeating node; each match becomes one row of {Out: value} pairs. The result is a
// docValue wrapping []any, one entry per row — the encoder decides how to render it.
type projection struct {
	rowPath string
	cols    []Column
}

// NewProjection builds a projection transform. rowPath is the path to the repeating element
// (e.g. "ListAllMyBucketsResult.Buckets.Bucket"); cols are the fields to pull from each.
func NewProjection(rowPath string, cols []Column) facade.Transform {
	return projection{rowPath: rowPath, cols: cols}
}

func (t projection) Apply(in facade.Page) (facade.Record, error) {
	doc, ok := in.Doc(facade.AnonymousPayload)
	if !ok {
		return nil, fmt.Errorf("transform: projection input is not an agnostic document")
	}
	m, ok := doc.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("transform: agnostic document is not a map")
	}

	rows, err := mxj.Map(m).ValuesForPath(t.rowPath)
	if err != nil {
		return nil, err
	}

	out := make([]any, 0, len(rows))
	for _, r := range rows {
		rm, ok := r.(map[string]interface{})
		if !ok {
			continue
		}
		sel := make(map[string]any, len(t.cols))
		for _, c := range t.cols {
			val, err := mxj.Map(rm).ValueForPath(c.Path)
			if err != nil {
				continue // absent column → omit from this row
			}
			sel[c.Out] = val
		}
		out = append(out, sel)
	}
	return record.NewRecord(map[string]facade.Value{
		facade.AnonymousPayload: value.NewDocValue(out),
	}), nil
}
