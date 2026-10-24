package docx

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"github.com/stackql-labs/omnisdk/internal/system_g/bind"
	"github.com/stackql-labs/omnisdk/internal/system_g/exchange"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/httpx"
	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
	"github.com/stackql-labs/omnisdk/pkg/encode"
	"github.com/stackql-labs/omnisdk/pkg/namespace"
)

type bodyShape struct{ shape aot.RequestShape }

// A read control belongs where the operation declares the parameter. A JSON
// API's limit is a body field even though OData uses query parameters for it.
func bodyControlSlot(request *httpx.Request, declaration aot.Request, wire, slot string) error {
	if len(request.Body.Raw) > 0 {
		var body map[string]any
		decoder := json.NewDecoder(bytes.NewReader(request.Body.Raw))
		decoder.UseNumber()
		if err := decoder.Decode(&body); err != nil {
			return fmt.Errorf("request read control %s: %w", wire, err)
		}
		request.Body = httpx.Body{Encoding: bodyEncoding(declaration.BodyMediaType()), Params: body}
	}
	if request.Body.Params == nil {
		request.Body = httpx.Body{Encoding: bodyEncoding(declaration.BodyMediaType()), Params: map[string]any{}}
	}
	request.Body.Params[wire] = "{" + slot + "}"
	return nil
}

func (b bodyShape) Apply(in facade.Page) (facade.Record, error) {
	raw := httpx.ReqBody(in)
	if len(raw) == 0 || b.shape.BodySchema() == nil {
		return httpx.NewRequestRecord(httpx.Method(in), httpx.URL(in), httpx.Header(in), raw), nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var body any
	if err := dec.Decode(&body); err != nil {
		return nil, err
	}
	schema := b.shape.BodySchema()
	m, object := body.(map[string]any)
	if !object {
		value, err := coerceBody("body", body, schema)
		if err != nil {
			return nil, err
		}
		payload, err := encode.JSON(value)
		if err != nil {
			return nil, err
		}
		return httpx.NewRequestRecord(httpx.Method(in), httpx.URL(in), httpx.Header(in), payload), nil
	}
	names := namespace.NewNaming(b.shape.NativeCasing(), b.shape.SnakeCaseAliases())
	if translation, ok := b.shape.(aot.BodyTranslationRequest); ok {
		if err := nestBodyInputs(m, schema, translation.BodyTranslation(), names); err != nil {
			return nil, err
		}
	}
	out := map[string]any{}
	for name, v := range m {
		wire, err := names.Native(name, schema.Properties())
		if err != nil {
			return nil, err
		}
		if _, exists := out[wire]; exists {
			return nil, fmt.Errorf("request body key %q supplied more than once through aliases", wire)
		}
		if property, exists := schema.Property(wire); exists {
			v, err = coerceBody(wire, v, property)
			if err != nil {
				return nil, err
			}
		}
		out[wire] = v
	}
	payload, err := encode.JSON(out)
	if err != nil {
		return nil, err
	}
	return httpx.NewRequestRecord(httpx.Method(in), httpx.URL(in), httpx.Header(in), payload), nil
}

func coerceBody(key string, v any, schema aot.Schema) (any, error) {
	value, err := coerceBodyValue(key, v, schema)
	if err != nil || value == nil {
		return value, err
	}
	if declared, ok := schema.(aot.StringOnlySchema); ok && declared.StringOnly() {
		if _, alreadyText := value.(string); !alreadyText {
			text, err := encode.JSON(value)
			return string(text), err
		}
	}
	return value, nil
}

func coerceBodyValue(key string, v any, schema aot.Schema) (any, error) {
	if v == nil {
		return nil, nil
	}
	typed, ok := schema.(aot.TypedSchema)
	if !ok {
		return v, nil
	}
	text, _ := encode.Text(v)
	fail := func() (any, error) {
		return nil, fmt.Errorf("request body key '%s': value '%s' cannot be coerced to schema type '%s'", key, text, typed.Type())
	}
	// SQL string literals can carry structured JSON, but only a valid value of
	// the schema's declared container type may enter the request body.
	if typed.Type() == "array" || typed.Type() == "object" {
		if text, ok := v.(string); ok {
			if !json.Valid([]byte(text)) {
				return fail()
			}
			decoder := json.NewDecoder(bytes.NewBufferString(text))
			decoder.UseNumber()
			if err := decoder.Decode(&v); err != nil {
				return fail()
			}
		}
	}
	switch typed.Type() {
	case "string":
		switch v.(type) {
		case string, json.Number, bool, int, int64, float64:
			return text, nil
		}
		return fail()
	case "boolean":
		if b, ok := v.(bool); ok {
			return b, nil
		}
		if s, ok := v.(string); ok {
			if b, err := strconv.ParseBool(s); err == nil {
				return b, nil
			}
		}
		return fail()
	case "number", "integer":
		switch v.(type) {
		case string, json.Number, int, int64, float64:
		default:
			return fail()
		}
		r, valid := new(big.Rat).SetString(text)
		if !valid || !json.Valid([]byte(text)) {
			return fail()
		}
		if typed.Type() == "integer" {
			if !r.IsInt() {
				return fail()
			}
			return json.Number(r.Num().String()), nil
		}
		return json.Number(text), nil
	case "object":
		m, ok := v.(map[string]any)
		if !ok {
			return fail()
		}
		out := map[string]any{}
		for name, value := range m {
			if p, ok := schema.Property(name); ok {
				var err error
				value, err = coerceBody(key+"."+name, value, p)
				if err != nil {
					return nil, err
				}
			}
			out[name] = value
		}
		return out, nil
	case "array":
		a, ok := v.([]any)
		if !ok {
			return fail()
		}
		out := append([]any(nil), a...)
		if item, ok := schema.Items(); ok {
			for i := range out {
				var err error
				out[i], err = coerceBody(fmt.Sprintf("%s[%d]", key, i), out[i], item)
				if err != nil {
					return nil, err
				}
			}
		}
		return out, nil
	}
	return v, nil
}

func nestBodyInputs(body map[string]any, schema aot.Schema, algorithm string, names namespace.Naming) error {
	if algorithm == "" || algorithm == "naive" || algorithm == "default" {
		return nil
	}
	parent, declared := strings.CutPrefix(algorithm, "naive_")
	if !declared || parent == "" {
		return fmt.Errorf("unsupported request body translation %q", algorithm)
	}
	nested, exists := schema.Property(parent)
	if !exists {
		return fmt.Errorf("request body translation %q names an undeclared property", algorithm)
	}
	values := map[string]any{}
	for name, value := range body {
		if _, topLevel := schema.Property(name); topLevel {
			continue
		}
		wire, err := names.Native(name, nested.Properties())
		if err != nil {
			return err
		}
		if _, matched := nested.Property(wire); !matched {
			continue
		}
		if _, duplicate := values[wire]; duplicate {
			return fmt.Errorf("request body key %q supplied more than once through aliases", wire)
		}
		values[wire] = value
		delete(body, name)
	}
	if len(values) > 0 {
		if _, supplied := body[parent]; supplied {
			return fmt.Errorf("request body property %q supplied both directly and through nested inputs", parent)
		}
		body[parent] = values
	}
	return nil
}

type rowNames struct {
	naming namespace.Naming
	scope  map[string]any
}

func (n rowNames) Apply(in facade.Page) (facade.Record, error) {
	doc, ok := in.Doc(facade.AnonymousPayload)
	if !ok {
		return nil, fmt.Errorf("response row has no document")
	}
	m, ok := doc.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("response row is not an object")
	}
	out := map[string]any{}
	for key, v := range m {
		name := n.naming.Surface(key)
		if _, exists := out[name]; exists {
			return nil, fmt.Errorf("response fields collide at SQL alias %q", name)
		}
		out[name] = v
	}
	// Native fields remain addressable by existing graph bindings; discovery and
	// SQL star expansion use the provider's declared presentation names.
	for key, v := range m {
		if _, exists := out[key]; !exists {
			out[key] = v
		}
	}
	// Request scope is not provider data. Add it after naming provider fields,
	// retaining native response names when a generated alias overlaps scope.
	for name, value := range n.scope {
		if _, providerField := m[name]; !providerField {
			out[name] = value
			out[n.naming.Surface(name)] = value
		}
	}
	return bind.NewDocRecord(out), nil
}

type rowScope struct{ scope map[string]any }

func (s rowScope) Apply(in facade.Page) (facade.Record, error) {
	doc, ok := in.Doc(facade.AnonymousPayload)
	if !ok {
		return nil, fmt.Errorf("response row has no document")
	}
	row, ok := doc.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("response row is not an object")
	}
	out := make(map[string]any, len(s.scope)+len(row))
	for name, value := range s.scope {
		out[name] = value
	}
	for name, value := range row {
		out[name] = value
	}
	return bind.NewDocRecord(out), nil
}

func namedRows(op facade.Operator, req aot.Request, bindings ...map[string]any) facade.Operator {
	scope := map[string]any{}
	if len(bindings) > 0 {
		for _, p := range req.Parameters() {
			if p.In() == aot.InPath || p.In() == aot.InQuery || p.In() == aot.InBody {
				if v, ok := bindings[0][p.Name()]; ok {
					scope[p.Name()] = v
				}
			}
		}
	}
	if shape, ok := req.(aot.RequestShape); ok && shape.SnakeCaseAliases() {
		return exchange.NewTransformExchange(0, op, rowNames{naming: namespace.NewNaming(shape.NativeCasing(), true), scope: scope}, 1)
	}
	if len(scope) > 0 {
		return exchange.NewTransformExchange(0, op, rowScope{scope}, 1)
	}
	return op
}
