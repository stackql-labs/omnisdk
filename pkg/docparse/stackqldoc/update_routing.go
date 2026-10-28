package stackqldoc

import (
	"bytes"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
	"gopkg.in/yaml.v3"
)

type updateDeclaration struct {
	resources               []string
	scope, target, location string
	priority, order         int
	requires, permits       []string
}
type updateRoute struct {
	resource, target, targetResource, source, location string
	priority, order                                    int
	requires, permits                                  []string
	exchange                                           aot.AOTExchange
}

func (r updateRoute) Resource() string          { return r.resource }
func (r updateRoute) Target() string            { return r.target }
func (r updateRoute) TargetResource() string    { return r.targetResource }
func (r updateRoute) Priority() int             { return r.priority }
func (r updateRoute) Requires() []string        { return slices.Clone(r.requires) }
func (r updateRoute) Permits() []string         { return slices.Clone(r.permits) }
func (r updateRoute) Source() string            { return r.source }
func (r updateRoute) Location() string          { return r.location }
func (r updateRoute) Order() int                { return r.order }
func (r updateRoute) Exchange() aot.AOTExchange { return r.exchange }

func routeReference(n *yaml.Node) (string, error) {
	if n == nil {
		return "", fmt.Errorf("missing reference")
	}
	if n.Kind == yaml.ScalarNode && n.Tag == "!!str" && n.Value != "" {
		return n.Value, nil
	}
	if n.Kind == yaml.MappingNode && len(n.Content) == 2 && n.Content[0].Value == "$ref" {
		return routeReference(n.Content[1])
	}
	return "", fmt.Errorf("expected a non-empty reference string or {$ref: ...}")
}
func routeAttributes(n *yaml.Node, optional bool) ([]string, error) {
	if n == nil && optional {
		return nil, nil
	}
	if n == nil || n.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("expected an attribute array")
	}
	var out []string
	for _, v := range n.Content {
		if v.Kind != yaml.ScalarNode || v.Tag != "!!str" || v.Value == "" || slices.Contains(out, v.Value) {
			return nil, fmt.Errorf("attributes must be unique non-empty strings")
		}
		out = append(out, v.Value)
	}
	return out, nil
}
func pointerEscape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}

// Only graph-bearing nodes are inspected. A response schema property named updates is ordinary data.
func parseUpdateDeclarations(data []byte) ([]updateDeclaration, error) {
	if !bytes.Contains(data, []byte("updates")) {
		return nil, nil
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, err
	}
	if len(root.Content) == 0 {
		return nil, nil
	}
	var out []updateDeclaration
	var walk func(*yaml.Node, string, string) error
	walk = func(n *yaml.Node, scope, location string) error {
		if n == nil || n.Kind != yaml.MappingNode {
			return nil
		}
		for i := 0; i < len(n.Content); i += 2 {
			k, v := n.Content[i].Value, n.Content[i+1]
			loc := location + "/" + pointerEscape(k)
			switch k {
			case "updates":
				if v.Kind != yaml.SequenceNode {
					return fmt.Errorf("%s must be an array", loc)
				}
				for j, item := range v.Content {
					where := fmt.Sprintf("%s/%d", loc, j)
					if item.Kind != yaml.MappingNode {
						return fmt.Errorf("%s must be an object", where)
					}
					keys := map[string]*yaml.Node{}
					for x := 0; x < len(item.Content); x += 2 {
						key := item.Content[x].Value
						if _, dup := keys[key]; dup {
							return fmt.Errorf("%s: duplicate %s", where, key)
						}
						switch key {
						case "resources", "priority", "requires", "permits", "target":
						default:
							return fmt.Errorf("%s: unknown field %s", where, key)
						}
						keys[key] = item.Content[x+1]
					}
					r := updateDeclaration{scope: scope, location: where, order: len(out)}
					p := keys["priority"]
					if p == nil || p.Tag != "!!int" {
						return fmt.Errorf("%s: integer priority is required", where)
					}
					if err := p.Decode(&r.priority); err != nil {
						return fmt.Errorf("%s: %w", where, err)
					}
					var err error
					if r.requires, err = routeAttributes(keys["requires"], true); err != nil {
						return fmt.Errorf("%s requires: %w", where, err)
					}
					if r.permits, err = routeAttributes(keys["permits"], false); err != nil {
						return fmt.Errorf("%s permits: %w", where, err)
					}
					for _, a := range r.requires {
						if !slices.Contains(r.permits, a) {
							return fmt.Errorf("%s: requires must be a subset of permits", where)
						}
					}
					if r.target, err = routeReference(keys["target"]); err != nil {
						return fmt.Errorf("%s target: %w", where, err)
					}
					if resources := keys["resources"]; resources != nil {
						nodes := []*yaml.Node{resources}
						if resources.Kind == yaml.SequenceNode {
							nodes = resources.Content
						}
						if len(nodes) == 0 {
							return fmt.Errorf("%s: resources must not be empty", where)
						}
						for _, node := range nodes {
							reference, err := routeReference(node)
							if err != nil {
								return fmt.Errorf("%s resources: %w", where, err)
							}
							if slices.Contains(r.resources, reference) {
								return fmt.Errorf("%s: duplicate resource binding", where)
							}
							r.resources = append(r.resources, reference)
						}
					} else if scope == "" {
						return fmt.Errorf("%s: resources are required outside a resource scope", where)
					}
					out = append(out, r)
				}
			case "config", "x-stackQL-config", "components":
				if err := walk(v, scope, loc); err != nil {
					return err
				}
			case "resources", "x-stackQL-resources":
				if v.Kind == yaml.MappingNode {
					for j := 0; j < len(v.Content); j += 2 {
						if err := walk(v.Content[j+1], v.Content[j].Value, loc+"/"+pointerEscape(v.Content[j].Value)); err != nil {
							return err
						}
					}
				}
			case "methods", "providerServices":
				if v.Kind == yaml.MappingNode {
					for j := 0; j < len(v.Content); j += 2 {
						if err := walk(v.Content[j+1], scope, loc+"/"+pointerEscape(v.Content[j].Value)); err != nil {
							return err
						}
					}
				}
			}
		}
		return nil
	}
	if err := walk(root.Content[0], "", "#"); err != nil {
		return nil, fmt.Errorf("stackqldoc updates: %w", err)
	}
	return out, nil
}

func (d *document) updateReference(reference string, method bool) (*document, string, string, error) {
	file, fragment, has := strings.Cut(reference, "#")
	if !has || !strings.HasPrefix(fragment, "/") {
		return nil, "", "", fmt.Errorf("update reference %q needs a document JSON pointer", reference)
	}
	target := d
	if file != "" {
		if d.resolver == nil {
			return nil, "", "", fmt.Errorf("update reference %q needs a bundle resolver", reference)
		}
		var err error
		target, err = d.resolver.ResolveDocument(reference, d.source)
		if err != nil {
			return nil, "", "", err
		}
	}
	if !method && (fragment == "/components/x-stackQL-resources" || fragment == "/resources") {
		return target, "", "", nil
	}
	parts := strings.Split(strings.TrimPrefix(fragment, "/"), "/")
	if len(parts) >= 3 && parts[0] == "components" && parts[1] == "x-stackQL-resources" {
		parts = parts[2:]
	} else if len(parts) >= 2 && parts[0] == "resources" {
		parts = parts[1:]
	} else {
		return nil, "", "", fmt.Errorf("update reference %q is not a resource pointer", reference)
	}
	res := unescapePointer(parts[0])
	if _, ok := target.Components.Resources[res]; !ok {
		return nil, "", "", fmt.Errorf("update reference %q names unknown resource %q", reference, res)
	}
	if !method && len(parts) == 1 {
		return target, res, "", nil
	}
	if method && len(parts) == 3 && parts[1] == "methods" && parts[2] != "" {
		return target, res, unescapePointer(parts[2]), nil
	}
	return nil, "", "", fmt.Errorf("update reference %q must name exactly a resource%s", reference, map[bool]string{true: " method", false: ""}[method])
}
func (d *document) UpdateRoutes(resource string) ([]aot.UpdateRoute, error) {
	if _, ok := d.Components.Resources[resource]; !ok {
		return nil, fmt.Errorf("stackqldoc: unknown resource %q", resource)
	}
	return d.resolveUpdateRoutes(resource, d.source, d.updates)
}
func (d *document) resolveUpdateRoutes(resource, boundSource string, declarations []updateDeclaration) ([]aot.UpdateRoute, error) {
	var out []aot.UpdateRoute
	for _, decl := range declarations {
		bound := decl.scope == resource && len(decl.resources) == 0
		for _, ref := range decl.resources {
			doc, res, _, err := d.updateReference(ref, false)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", decl.location, err)
			}
			if (res == resource || (res == "" && doc.Components.Resources[resource].Methods != nil)) && doc.source == boundSource {
				bound = true
			}
		}
		if !bound {
			continue
		}
		target, res, method, err := d.updateReference(decl.target, true)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", decl.location, err)
		}
		exchange, err := target.resolve(res, method, true)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", decl.location, err)
		}
		file := target.source
		source := d.source
		out = append(out, updateRoute{resource: resource, target: file + "#/components/x-stackQL-resources/" + pointerEscape(res) + "/methods/" + pointerEscape(method), targetResource: res, source: source, location: decl.location, priority: decl.priority, order: decl.order, requires: decl.requires, permits: decl.permits, exchange: exchange})
	}
	return out, nil
}

// UpdateRoutes combines provider- and service-document recommendations without overriding scopes.
func (c *catalog) UpdateRoutes(address string) ([]aot.UpdateRoute, error) {
	_, service, resource, err := c.split(address)
	if err != nil {
		return nil, err
	}
	parsed, err := c.doc(service)
	if err != nil {
		return nil, err
	}
	d, ok := parsed.(*document)
	if !ok {
		return nil, fmt.Errorf("stackqldoc: document does not expose update routing")
	}
	routes, err := d.UpdateRoutes(resource)
	if err != nil {
		return nil, err
	}
	if p, ok := c.provider.(*provider); ok && len(p.updates) > 0 {
		providerDoc := &document{source: "provider.yaml", resolver: bundleResolver{catalog: c}}
		inherited, err := providerDoc.resolveUpdateRoutes(resource, d.source, p.updates)
		if err != nil {
			return nil, err
		}
		routes = append(routes, inherited...)
	}
	out := make([]aot.UpdateRoute, 0, len(routes))
	for _, r := range routes {
		v := r.(updateRoute)
		v.resource = address
		v.source = path.Join(c.provider.Name(), c.provider.Version(), v.source)
		file, _, _ := strings.Cut(v.target, "#")
		targetService := ""
		for _, svc := range c.Services() {
			if c.files[svc] == file {
				targetService = svc
				break
			}
		}
		if targetService == "" {
			return nil, fmt.Errorf("update target %s is not an addressable service resource", v.target)
		}
		v.targetResource = c.address(targetService, v.targetResource)
		out = append(out, v)
	}
	return out, nil
}
