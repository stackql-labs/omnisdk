// Package starlark implements bounded, pure Starlark transforms and predicates.
package starlark

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"

	"github.com/stackql-labs/omnisdk/pkg/docparse/dsl"
	star "go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"
	"go.starlark.net/syntax"
)

const Type = "starlark"
const MaxSteps = 100000

type evaluator struct{}

func New() dsl.Evaluator       { return evaluator{} }
func (evaluator) Type() string { return Type }

func (evaluator) Eval(ctx dsl.Context, program string, in []byte) ([]byte, error) {
	if err := Validate(program); err != nil {
		return nil, err
	}
	if len(in) > MaxValueBytes {
		return nil, fmt.Errorf("Starlark inbox exceeds %d bytes", MaxValueBytes)
	}
	var input any
	decoder := json.NewDecoder(bytes.NewReader(in))
	decoder.UseNumber()
	if err := decoder.Decode(&input); err != nil {
		return nil, err
	}
	inbox, err := toValue(input)
	if err != nil {
		return nil, err
	}
	inbox.Freeze()
	edgeBytes, err := json.Marshal(ctx.Edges)
	if err != nil {
		return nil, err
	}
	if len(edgeBytes) > MaxValueBytes {
		return nil, fmt.Errorf("Starlark edge snapshots exceed size limit")
	}
	edges := star.NewDict(len(ctx.Edges))
	for name, snapshot := range ctx.Edges {
		fields := star.StringDict{}
		for key, value := range snapshot {
			converted, err := toValue(value)
			if err != nil {
				return nil, err
			}
			fields[key] = converted
		}
		if err := edges.SetKey(star.String(name), starlarkstruct.FromStringDict(star.String("edge"), fields)); err != nil {
			return nil, err
		}
	}
	edges.Freeze()
	globals := star.StringDict{"inbox": inbox, "edges": edges, "re": regexModule()}
	thread := &star.Thread{Name: "transform"}
	thread.SetMaxExecutionSteps(MaxSteps)
	execution := ctx.Execution
	if execution == nil {
		execution = context.Background()
	}
	if err := execution.Err(); err != nil {
		return nil, err
	}
	stop := context.AfterFunc(execution, func() { thread.Cancel(execution.Err().Error()) })
	defer stop()
	var result star.Value
	if _, parseErr := syntax.ParseExpr("program.star", program, 0); parseErr == nil {
		result, err = star.Eval(thread, "program.star", program, globals)
	} else {
		var module star.StringDict
		module, err = star.ExecFile(thread, "program.star", program, globals)
		if err == nil {
			result = module["result"]
			if result == nil {
				err = fmt.Errorf("program must assign result")
			}
		}
	}
	if err != nil {
		return nil, err
	}
	output, err := fromValue(result)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(output)
	if len(encoded) > MaxValueBytes {
		return nil, fmt.Errorf("Starlark output exceeds size limit")
	}
	return encoded, err
}

func regexModule() *starlarkstruct.Module {
	return &starlarkstruct.Module{Name: "re", Members: star.StringDict{
		"search":    star.NewBuiltin("re.search", regex(false)),
		"fullmatch": star.NewBuiltin("re.fullmatch", regex(true)),
	}}
}
func regex(full bool) func(*star.Thread, *star.Builtin, star.Tuple, []star.Tuple) (star.Value, error) {
	return func(_ *star.Thread, fn *star.Builtin, args star.Tuple, kwargs []star.Tuple) (star.Value, error) {
		var pattern, text string
		if err := star.UnpackArgs(fn.Name(), args, kwargs, "pattern", &pattern, "text", &text); err != nil {
			return nil, err
		}
		if len(text) > 65536 {
			return nil, fmt.Errorf("regexp text exceeds 65536 bytes")
		}
		if full {
			pattern = "\\A(?:" + pattern + ")\\z"
		}
		compiled, err := regexp.Compile(pattern)
		if err != nil {
			return nil, err
		}
		return star.Bool(compiled.MatchString(text)), nil
	}
}
func toValue(value any) (star.Value, error) { return toValueDepth(value, 0) }
func toValueDepth(value any, depth int) (star.Value, error) {
	if depth > 64 {
		return nil, fmt.Errorf("Starlark input nesting exceeds 64 levels")
	}
	switch v := value.(type) {
	case nil:
		return star.None, nil
	case bool:
		return star.Bool(v), nil
	case string:
		return star.String(v), nil
	case json.Number:
		if integer, ok := new(big.Int).SetString(string(v), 10); ok {
			return star.MakeBigInt(integer), nil
		}
		f, err := v.Float64()
		return star.Float(f), err
	case map[string]any:
		d := star.NewDict(len(v))
		for key, item := range v {
			converted, err := toValueDepth(item, depth+1)
			if err != nil {
				return nil, err
			}
			if err := d.SetKey(star.String(key), converted); err != nil {
				return nil, err
			}
		}
		return d, nil
	case []any:
		values := make([]star.Value, len(v))
		for i, item := range v {
			converted, err := toValueDepth(item, depth+1)
			if err != nil {
				return nil, err
			}
			values[i] = converted
		}
		return star.NewList(values), nil
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		var normalized any
		d := json.NewDecoder(bytes.NewReader(encoded))
		d.UseNumber()
		if err := d.Decode(&normalized); err != nil {
			return nil, err
		}
		return toValueDepth(normalized, depth+1)
	}
}
func fromValue(value star.Value) (any, error) {
	budget := MaxValueBytes
	return fromValueBounded(value, &budget, 0)
}

func fromValueBounded(value star.Value, budget *int, depth int) (any, error) {
	*budget -= 8
	if *budget < 0 || depth > 64 {
		return nil, fmt.Errorf("Starlark output exceeds resource limits")
	}
	switch v := value.(type) {
	case star.NoneType:
		return nil, nil
	case star.Bool:
		return bool(v), nil
	case star.String:
		*budget -= len(v)
		if *budget < 0 {
			return nil, fmt.Errorf("Starlark output exceeds size limit")
		}
		return string(v), nil
	case star.Int:
		text := v.String()
		*budget -= len(text)
		if *budget < 0 {
			return nil, fmt.Errorf("Starlark output exceeds size limit")
		}
		return json.Number(text), nil
	case star.Float:
		return float64(v), nil
	case *star.Dict:
		out := map[string]any{}
		for _, pair := range v.Items() {
			key, ok := star.AsString(pair[0])
			if !ok {
				return nil, fmt.Errorf("object keys must be strings")
			}
			*budget -= len(key)
			if *budget < 0 {
				return nil, fmt.Errorf("Starlark output exceeds size limit")
			}
			item, err := fromValueBounded(pair[1], budget, depth+1)
			if err != nil {
				return nil, err
			}
			out[key] = item
		}
		return out, nil
	case *star.List:
		out := make([]any, v.Len())
		for i := range out {
			item, err := fromValueBounded(v.Index(i), budget, depth+1)
			if err != nil {
				return nil, err
			}
			out[i] = item
		}
		return out, nil
	case star.Tuple:
		out := make([]any, len(v))
		for i, item := range v {
			converted, err := fromValueBounded(item, budget, depth+1)
			if err != nil {
				return nil, err
			}
			out[i] = converted
		}
		return out, nil
	default:
		return nil, fmt.Errorf("cannot return %s as JSON", value.Type())
	}
}
