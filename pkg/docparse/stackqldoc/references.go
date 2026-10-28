package stackqldoc

import (
	"fmt"
	"io/fs"
	"path"
	"strings"

	"gopkg.in/yaml.v3"
)

// documentResolver reads references within the provider bundle. No remote fetches
// or references outside that bundle participate in resolving an operation.
type documentResolver interface {
	ResolveDocument(reference, origin string) (*document, error)
}

type bundleResolver struct{ catalog *catalog }

func bundlePath(reference, origin string) (string, error) {
	file, _, _ := strings.Cut(reference, "#")
	if file == "" {
		return origin, nil
	}
	if strings.Contains(file, ":") || strings.HasPrefix(file, "/") || strings.Contains(file, "\\") {
		return "", fmt.Errorf("stackqldoc: invalid bundle reference %q", reference)
	}
	// Registry references include the provider/version prefix; the filesystem is
	// already rooted at that version. Keep the declared directory, not its basename.
	parts := strings.Split(file, "/")
	if len(parts) >= 3 && (parts[2] == "services" || parts[2] == "resources" || parts[2] == "services-split") {
		file = strings.Join(parts[2:], "/")
	} else if !strings.HasPrefix(file, "services/") && !strings.HasPrefix(file, "resources/") && !strings.HasPrefix(file, "services-split/") {
		file = path.Join(path.Dir(origin), file)
	}
	file = path.Clean(file)
	if !fs.ValidPath(file) || file == "." {
		return "", fmt.Errorf("stackqldoc: reference %q escapes the provider bundle", reference)
	}
	return file, nil
}

func (r bundleResolver) ResolveDocument(reference, origin string) (*document, error) {
	file, err := bundlePath(reference, origin)
	if err != nil {
		return nil, err
	}
	var key string
	aliases := false
	if p, ok := r.catalog.provider.(*provider); ok {
		aliases = p.doc.Config.SnakeCaseAliases
	}
	if r.catalog.docs != nil {
		if info, err := fs.Stat(r.catalog.fsys, file); err == nil {
			key = fmt.Sprintf("%s/operation/%s\x00%d\x00%d\x00%t", r.catalog.docKey, file, info.Size(), info.ModTime().UnixNano(), aliases)
			if cached, ok := r.catalog.docs.Get(key); ok {
				if parsed, ok := cached.(*document); ok {
					clone := *parsed
					clone.source, clone.resolver = file, r
					return &clone, nil
				}
			}
		}
	}
	b, err := r.catalog.settings.readDocument(r.catalog.fsys, file)
	if err != nil {
		return nil, fmt.Errorf("stackqldoc: read referenced document %s: %w", file, err)
	}
	var d document
	if err := yaml.Unmarshal(b, &d); err != nil {
		return nil, fmt.Errorf("stackqldoc: parse referenced document %s: %w", file, err)
	}
	if d.updates, err = parseUpdateDeclarations(b); err != nil {
		return nil, err
	}
	d.source, d.resolver = file, r
	d.providerName = r.catalog.provider.Name()
	if p, ok := r.catalog.provider.(*provider); ok {
		d.snakeAliases = p.doc.Config.SnakeCaseAliases
	}
	if len(d.Components.Resources) == 0 {
		d.Components.Resources = d.ResourcesRoot
	}
	if key != "" {
		r.catalog.docs.Put(key, &d, int64(len(b))*docCost)
	}
	clone := d
	return &clone, nil
}

// operationDocument chooses the document owning an operation. Resource-only
// documents attach their resource declarations to the referenced OpenAPI document;
// schemas and path-level parameters therefore resolve in their original document.
func (d *document) operationDocument(name, methodName string, m method) (*document, method, error) {
	file, fragment, external := strings.Cut(m.Operation.Ref, "#")
	if !external || file == "" {
		if d.ServiceDoc.Ref == "" {
			return d, m, nil
		}
		file = d.ServiceDoc.Ref
		if !external {
			fragment = m.Operation.Ref
		}
	}
	if d.resolver == nil {
		return nil, m, fmt.Errorf("stackqldoc: operation %q requires a bundle resolver for %q", methodName, file)
	}
	target, err := d.resolver.ResolveDocument(file, d.source)
	if err != nil {
		return nil, m, err
	}
	// The explicit file owns this local pointer. Do not follow another
	// serviceDoc when resolving it (including cyclic wrapper references).
	target.ServiceDoc = ref{}
	m.Operation.Ref = "#" + fragment
	res := d.Components.Resources[name]
	methods := make(map[string]method, len(res.Methods))
	for key, value := range res.Methods {
		methods[key] = value
	}
	methods[methodName] = m
	res.Methods = methods
	resources := make(map[string]resource, len(d.Components.Resources))
	for key, resource := range d.Components.Resources {
		resources[key] = resource
	}
	resources[name] = res
	target.Components.Resources = resources
	return target, m, nil
}
