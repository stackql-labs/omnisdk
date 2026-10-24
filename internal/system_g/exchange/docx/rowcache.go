package docx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/stackql-labs/omnisdk/internal/system_g/exchange"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/httpx"
	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
)

// WithRowCache caches complete transformed reads, before input echoing and SQL.
// Callers must exclude mutations, polls and externally capped reads.
func WithRowCache(cache facade.RowCache, partition, resource string) Option {
	return func(o *options) { o.rowCache = cache; o.cachePartition = partition; o.cacheResource = resource }
}

type rowCachePlan struct {
	cache     facade.RowCache
	key       facade.RowCacheKey
	bindings  []string
	authToken bool
}

func cacheRowsPlan(ex aot.AOTExchange, resp aot.Response, request httpx.Request, bindings []string, authToken bool, o options) rowCachePlan {
	if o.rowCache == nil || o.wholeResponse || o.poll != nil || o.queryLimit > 0 {
		return rowCachePlan{}
	}
	identity, ok := ex.(aot.DocumentIdentity)
	if !ok || identity.DocumentIdentity() == "" {
		return rowCachePlan{}
	}
	// Store only the digest of transport/program metadata, never raw credentials.
	metadata := map[string]any{
		"version": "omni-row-v1", "request": request, "objectKey": resp.ObjectKey(),
		"mediaType": resp.MediaType(), "overrideMediaType": resp.OverrideMediaType(),
		"programType": resp.Transform().Type(), "program": resp.Transform().Body(),
		"translate": ex.Request().Translate(), "transpose": ex.Request().Transpose(),
		"schema": cacheSchema(resp.Schema(), 0),
	}
	if selected, ok := resp.(aot.SelectedSchema); ok {
		metadata["rowSchema"] = cacheSchema(selected.RowSchema(), 0)
	}
	cacheRequestMetadata(metadata, ex.Request())
	if gx, ok := ex.(aot.GraphQLExchange); ok {
		if g, present := gx.GraphQL(); present {
			metadata["graphql"] = []any{g.URL(), g.HTTPVerb(), g.Query(), g.Selection(), g.CursorPath(), g.CursorStrategy(), g.CursorFormat(), g.TerminationPath(), g.PageSize()}
		}
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return rowCachePlan{}
	}
	names := slices.Clone(bindings)
	for name := range o.provided {
		names = withInput(names, name)
	}
	sum := sha256.Sum256(encoded)
	return rowCachePlan{cache: o.rowCache, key: facade.RowCacheKey{
		Partition: o.cachePartition, Resource: o.cacheResource, Method: ex.Name(),
		Document: identity.DocumentIdentity(), Request: hex.EncodeToString(sum[:]),
	}, bindings: names, authToken: authToken}
}
func (p rowCachePlan) wrap(op facade.Operator, bound map[string]any) facade.Operator {
	if p.cache == nil {
		return op
	}
	key := p.key
	key.Inputs = map[string]any{}
	for _, name := range p.bindings {
		if p.authToken && name == TokenAttr {
			continue
		}
		if value, present := bound[name]; present {
			key.Inputs[name] = value
		}
	}
	if _, err := json.Marshal(key.Inputs); err != nil {
		if observer, ok := p.cache.(facade.RowCacheErrorObserver); ok {
			observer.CacheError(fmt.Errorf("row cache inputs: %w", err))
		}
		return op
	}
	return uncappedCacheRows{raw: op, cached: exchange.NewCachedRows(op, p.cache, key)}
}

type uncappedCacheRows struct{ raw, cached facade.Operator }

func (o uncappedCacheRows) Open(ctx context.Context) facade.Records {
	if httpx.PageLimit(ctx) > 0 {
		return o.raw.Open(ctx)
	}
	return o.cached.Open(ctx)
}

// Include resolved schemas and request transforms: their declarations may have
// come from referenced documents rather than the exchange's immediate source.
func cacheSchema(schema aot.Schema, depth int) any {
	if schema == nil {
		return nil
	}
	if depth > 32 {
		return "depth-bound"
	}
	out := map[string]any{"wire": schema.WireName()}
	if typed, ok := schema.(aot.TypedSchema); ok {
		out["type"] = typed.Type()
		out["format"] = typed.Format()
	}
	if scalar, ok := schema.(aot.StringOnlySchema); ok {
		out["stringOnly"] = scalar.StringOnly()
	}
	if required, ok := schema.(aot.RequiredSchema); ok {
		out["required"] = required.RequiredProperties()
	}
	props := map[string]any{}
	for _, name := range schema.Properties() {
		if property, present := schema.Property(name); present {
			props[name] = cacheSchema(property, depth+1)
		}
	}
	out["properties"] = props
	if item, present := schema.Items(); present {
		out["items"] = cacheSchema(item, depth+1)
	}
	return out
}
func cacheRequestMetadata(metadata map[string]any, request aot.Request) {
	if transform, ok := request.(aot.RequestTransform); ok && transform.RequestTransform() != nil {
		program := transform.RequestTransform()
		metadata["requestProgram"] = []string{program.Type(), program.Body()}
	}
	if shape, ok := request.(aot.RequestShape); ok {
		metadata["bodySchema"] = cacheSchema(shape.BodySchema(), 0)
		metadata["nativeCasing"] = shape.NativeCasing()
		metadata["snakeAliases"] = shape.SnakeCaseAliases()
	}
	if body, ok := request.(aot.BodyTranslationRequest); ok {
		metadata["bodyTranslation"] = body.BodyTranslation()
	}
	if xml, ok := request.(aot.XMLRequest); ok {
		metadata["xml"] = []string{xml.XMLRootName(), xml.XMLDeclaration(), xml.XMLRootAnnotation()}
	}
}
