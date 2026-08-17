package stackqldoc

import (
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync"

	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
)

// Option configures how a bundle is resolved.
type Option func(*settings)

type settings struct{ prefix string }

// WithProviderPrefix sets the namespace document-derived addresses live under. Empty means none — a
// caller that treats the documents as authoritative can drop it deliberately, which is different from
// forgetting it was there.
func WithProviderPrefix(prefix string) Option {
	return func(s *settings) { s.prefix = prefix }
}

func resolveSettings(opts []Option) settings {
	s := settings{prefix: aot.DefaultProviderPrefix}
	for _, o := range opts {
		o(&s)
	}
	return s
}

// Open resolves a provider bundle — provider.yaml plus the service documents beside it — into
// addressable exchanges. Service documents are parsed LAZILY: a provider lists hundreds and each is
// megabytes, so resolving an address must not cost the whole catalogue.
func Open(fsys fs.FS, opts ...Option) (aot.Catalog, error) {
	b, err := fs.ReadFile(fsys, "provider.yaml")
	if err != nil {
		return nil, fmt.Errorf("stackqldoc: open provider: %w", err)
	}
	p, err := ParseProvider(b)
	if err != nil {
		return nil, err
	}
	c := &catalog{fsys: fsys, provider: p, files: map[string]string{}, prefix: resolveSettings(opts).prefix}
	// A provider lists every service it could offer; only those whose document is present here are
	// addressable. Which is which is a fact about the bundle, so it is settled once, up front.
	for _, s := range p.Services() {
		file := path.Join("services", path.Base(s.Ref()))
		if _, err := fs.Stat(fsys, file); err == nil {
			c.files[s.Name()] = file
		}
	}
	return c, nil
}

// A catalog holds NO parsed documents. A query spans several services and several providers, so
// caching documents would make the working set the sum of every document touched — the largest thing
// in play — instead of the exchanges actually needed. A document is parsed, its exchange resolved,
// and the document dropped; a resolved exchange is self-contained (templates and programs, a few KB)
// and never refers back to it. So planning is the release point and execution holds no documents at
// all.
type catalog struct {
	fsys     fs.FS
	provider aot.Provider

	prefix string // the namespace addresses live under

	mu    sync.Mutex
	files map[string]string // service → document path, for services actually present
}

func (c *catalog) Provider() aot.Provider { return c.provider }

func (c *catalog) Services() []string {
	out := make([]string, 0, len(c.files))
	for name := range c.files {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func (c *catalog) Resources(service string) ([]string, error) {
	doc, err := c.doc(service)
	if err != nil {
		return nil, err
	}
	return doc.Resources(), nil
}

func (c *catalog) Methods(service, resource string) ([]aot.Method, error) {
	doc, err := c.doc(service)
	if err != nil {
		return nil, err
	}
	return doc.Methods(resource)
}

func (c *catalog) Paths() []string {
	var out []string
	for _, svc := range c.Services() {
		// Parse and DISCARD. A sweep touches every service, and retaining them all defeats the point
		// of an on-demand catalog: the whole bundle parsed is ~65MB of live heap, carried forever for
		// a listing the caller asked for once. Only a service someone actually addresses stays
		// resident.
		doc, err := c.doc(svc)
		if err != nil {
			continue // a service whose document will not parse contributes no addresses
		}
		for _, res := range doc.Resources() {
			if _, err := doc.Select(res); err != nil {
				continue // no SELECT: nothing to address yet
			}
			out = append(out, c.address(svc, res))
		}
	}
	sort.Strings(out)
	return out
}

func (c *catalog) Exchanges(addr string) ([]aot.AOTExchange, error) {
	prov, svc, res, err := c.split(addr)
	if err != nil {
		return nil, err
	}
	if prov != c.prefix+c.provider.Name() {
		return nil, fmt.Errorf("stackqldoc: address %q is not for provider %q", addr, c.prefix+c.provider.Name())
	}
	doc, err := c.doc(svc)
	if err != nil {
		return nil, err
	}
	return doc.Selects(res)
}

func (c *catalog) Exchange(addr string) (aot.AOTExchange, error) {
	prov, svc, res, err := c.split(addr)
	if err != nil {
		return nil, err
	}
	if prov != c.prefix+c.provider.Name() {
		return nil, fmt.Errorf("stackqldoc: address %q is not for provider %q", addr,
			c.prefix+c.provider.Name())
	}
	doc, err := c.doc(svc)
	if err != nil {
		return nil, err
	}
	return doc.Select(res)
}

// address is the dot-path for a resource: the document's own provider/service/resource, under the
// document-derived namespace.
func (c *catalog) address(service, resource string) string {
	return c.prefix + c.provider.Name() + "." + service + "." + resource
}

// split parses an address. A resource name may itself contain dots, so only the first two segments
// are fixed.
func (c *catalog) split(addr string) (provider, service, resource string, err error) {
	parts := strings.SplitN(addr, ".", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", fmt.Errorf("stackqldoc: address %q is not <provider>.<service>.<resource>", addr)
	}
	return parts[0], parts[1], parts[2], nil
}

// doc reads and parses a service document, retaining nothing. The caller keeps what it resolved, not
// what it was resolved from.
func (c *catalog) doc(service string) (Doc, error) {
	c.mu.Lock()
	file, ok := c.files[service]
	known := c.servicesLocked()
	c.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("stackqldoc: service %q has no document in this bundle (have %d services)",
			service, len(known))
	}
	b, err := fs.ReadFile(c.fsys, file)
	if err != nil {
		return nil, fmt.Errorf("stackqldoc: read %s: %w", file, err)
	}
	return Parse(b)
}

func (c *catalog) servicesLocked() []string {
	out := make([]string, 0, len(c.files))
	for name := range c.files {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
