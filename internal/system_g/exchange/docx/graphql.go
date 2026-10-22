package docx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"text/template"

	"github.com/stackql-labs/omnisdk/internal/system_g/buffer"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/httpx"
	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
	"github.com/stackql-labs/omnisdk/pkg/jsonpath"
)

type graphQLOperator struct {
	spec                           aot.GraphQL
	req                            httpx.Request
	bound                          map[string]any
	transforms                     []facade.Transform
	query, format                  *template.Template
	selection, cursor, termination jsonpath.Selection
	limit                          int
	err                            error
}

func newGraphQLOperator(spec aot.GraphQL, fallback string, req httpx.Request, bound map[string]any, transforms []facade.Transform, limit int, base string) facade.Operator {
	g := &graphQLOperator{spec: spec, req: req, bound: bound, transforms: transforms, limit: limit}
	if limit <= 0 {
		g.limit = 100
	}
	selection := spec.Selection()
	if selection == "" {
		selection = fallback
	}
	g.selection, g.err = jsonpath.Parse(selection)
	if g.err == nil {
		g.query, g.err = template.New("graphql").Option("missingkey=error").Parse(spec.Query())
	}
	if g.err == nil && spec.Query() == "" {
		g.err = fmt.Errorf("graphql: operation has no query")
	}
	if g.err == nil && spec.CursorPath() != "" {
		g.cursor, g.err = jsonpath.Parse(spec.CursorPath())
	}
	if g.err == nil && spec.TerminationPath() != "" {
		g.termination, g.err = jsonpath.Parse(spec.TerminationPath())
	}
	if g.err == nil && spec.CursorFormat() != "" {
		g.format, g.err = template.New("cursor").Option("missingkey=error").Parse(spec.CursorFormat())
	}
	switch spec.CursorStrategy() {
	case "", "cursor_after", "keyset", "offset", "page_info":
	default:
		g.err = fmt.Errorf("graphql: unsupported cursor strategy %q", spec.CursorStrategy())
	}
	if spec.CursorStrategy() == "page_info" && (g.cursor == nil || g.termination == nil) {
		g.err = fmt.Errorf("graphql: page_info requires cursor and termination paths")
	}
	if spec.CursorStrategy() == "keyset" && g.cursor == nil {
		g.err = fmt.Errorf("graphql: keyset requires a cursor path")
	}
	if spec.URL() != "" {
		g.req.URL = retarget(spec.URL(), base)
	}
	if spec.HTTPVerb() != "" {
		g.req.Method = strings.ToUpper(spec.HTTPVerb())
	}
	g.req.Continuation = httpx.Continuation{}
	// Substitution occurs once, so GraphQL braces inside the bound query are
	// literal data rather than HTTP placeholder syntax.
	g.req.Body = httpx.Body{Encoding: httpx.EncodingJSON, Params: map[string]any{"query": "{__omni_graphql_query}"}}
	return g
}

func (g *graphQLOperator) Open(ctx context.Context) facade.Records {
	buf := buffer.NewBuffer(1, 1024, buffer.PagesAhead(ctx))
	go func() { buf.Complete(g.run(ctx, buf)) }()
	return buf.Reader()
}

func (g *graphQLOperator) run(ctx context.Context, buf facade.Buffer) error {
	if g.err != nil {
		return g.err
	}
	fragment := ""
	offset := 0
	seen := map[string]bool{}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		args := make(map[string]any, len(g.bound)+2)
		for k, v := range g.bound {
			args[k] = v
		}
		args["limit"], args["cursor"] = g.limit, fragment
		var query bytes.Buffer
		if err := g.query.Execute(&query, args); err != nil {
			return fmt.Errorf("graphql query template: %w", err)
		}
		bound := make(map[string]any, len(g.bound)+1)
		for k, v := range g.bound {
			bound[k] = v
		}
		bound["__omni_graphql_query"] = query.String()
		rec, doc, err := g.page(ctx, bound)
		if err != nil {
			return err
		}
		selected := flattenSelection(g.selection.Values(doc))
		if len(selected) == 0 {
			return nil
		}
		if err := buf.Append(ctx, rec); err != nil {
			if errors.Is(err, buffer.ErrAllReadersClosed) {
				return nil
			}
			return err
		}
		if g.termination != nil {
			flags := g.termination.Values(doc)
			if len(flags) != 1 {
				return fmt.Errorf("graphql: termination path must select one boolean")
			}
			more, ok := flags[0].(bool)
			if !ok {
				return fmt.Errorf("graphql: termination path is not boolean")
			}
			if !more {
				return nil
			}
		}
		var value any
		if g.spec.CursorStrategy() == "offset" {
			if g.spec.PageSize() > 0 && len(selected) < g.spec.PageSize() {
				return nil
			}
			offset += len(selected)
			value = offset
		} else {
			if g.cursor == nil {
				return nil
			}
			values := g.cursor.Values(doc)
			if len(values) == 0 || values[len(values)-1] == nil {
				if g.termination != nil {
					return fmt.Errorf("graphql: hasNextPage is true but the cursor is missing")
				}
				return nil
			}
			value = values[len(values)-1]
			if text, ok := value.(string); ok && text == "" {
				if g.termination != nil {
					return fmt.Errorf("graphql: hasNextPage is true but the cursor is empty")
				}
				return nil
			}
			switch value.(type) {
			case string, json.Number, float64, int:
			default:
				return fmt.Errorf("graphql: cursor is not a scalar")
			}
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		identity := string(encoded)
		if seen[identity] {
			return fmt.Errorf("graphql: cursor did not advance")
		}
		seen[identity] = true
		if g.format != nil {
			var cursor bytes.Buffer
			if err := g.format.Execute(&cursor, map[string]any{"value": value}); err != nil {
				return fmt.Errorf("graphql cursor template: %w", err)
			}
			fragment = cursor.String()
		} else {
			fragment = ", after: " + identity
		}
	}
}

func (g *graphQLOperator) page(ctx context.Context, bound map[string]any) (facade.Record, map[string]any, error) {
	rows := httpx.Make(g.req, nil, g.transforms...)(bound).Open(ctx)
	defer rows.Close()
	if !rows.Next(ctx) {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		return nil, nil, rows.Err()
	}
	rec := rows.Record()
	if _, err := httpx.NewRequireOK().Apply(rec); err != nil {
		return nil, nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(rec.Bytes(facade.AnonymousPayload)))
	decoder.UseNumber()
	var doc map[string]any
	if err := decoder.Decode(&doc); err != nil {
		return nil, nil, fmt.Errorf("graphql response: %w", err)
	}
	if problems, ok := doc["errors"].([]any); ok && len(problems) > 0 {
		return nil, nil, fmt.Errorf("graphql response errors: %v", problems)
	}
	if rows.Next(ctx) {
		return nil, nil, fmt.Errorf("graphql: expected one HTTP response per page")
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return rec, doc, nil
}

func flattenSelection(values []any) []any {
	out := []any{}
	for _, value := range values {
		if list, ok := value.([]any); ok {
			out = append(out, list...)
		} else if value != nil {
			out = append(out, value)
		}
	}
	return out
}

type selectionRows struct{ selection jsonpath.Selection }

func (s selectionRows) Apply(in facade.Page) (facade.Record, error) {
	doc, ok := in.Doc(facade.AnonymousPayload)
	if !ok {
		return nil, fmt.Errorf("response selection requires a decoded document")
	}
	return docRecord(flattenSelection(s.selection.Values(doc))), nil
}
