package docx

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"

	"github.com/stackql-labs/omnisdk/internal/system_g/bind"
	"github.com/stackql-labs/omnisdk/internal/system_g/exchange"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/httpx"
	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
	"github.com/stackql-labs/omnisdk/pkg/encode"
	"github.com/stackql-labs/omnisdk/pkg/namespace"
)

type bodyShape struct{ shape aot.RequestShape }

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

type rowNames struct{ naming namespace.Naming }

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
	if len(bindings) > 0 {
		scope := map[string]any{}
		for _, p := range req.Parameters() {
			if p.In() == aot.InPath || p.In() == aot.InQuery || p.In() == aot.InBody {
				if v, ok := bindings[0][p.Name()]; ok {
					scope[p.Name()] = v
				}
			}
		}
		if len(scope) > 0 {
			op = exchange.NewTransformExchange(0, op, rowScope{scope}, 1)
		}
	}
	shape, ok := req.(aot.RequestShape)
	if !ok || !shape.SnakeCaseAliases() {
		return op
	}
	return exchange.NewTransformExchange(0, op, rowNames{namespace.NewNaming(shape.NativeCasing(), true)}, 1)
}
