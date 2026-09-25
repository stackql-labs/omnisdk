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

type settings struct {
	prefix string
	docs   DocCache
	docKey string
}

// DocCache holds parsed service documents across catalogs. A parsed document is read-only, so one
// entry serves every concurrent reader.
type DocCache interface {
	Get(key string) (Doc, bool)
	Put(key string, d Doc, cost int64)
}

// WithDocCache keeps parsed documents in c, keyed under root — the directory the documents are
// read from. Two views of a registry are two roots, so a client's patched documents never serve
// another client. A document's key includes its size and modification time, so a rewritten file is
// parsed afresh.
func WithDocCache(c DocCache, root string) Option {
	return func(s *settings) { s.docs, s.docKey = c, root }
}

// underKey places a catalog's documents beneath its directory within the cache's root.
func underKey(dir string) Option {
	return func(s *settings) { s.docKey = path.Join(s.docKey, dir) }
}

// docCost estimates a parsed document's footprint from its source size: decoded YAML carries
// several times its text in node and map overhead.
const docCost = 4

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
	st := resolveSettings(opts)
	c := &catalog{fsys: fsys, provider: p, files: map[string]string{}, prefix: st.prefix, docs: st.docs, docKey: st.docKey}
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

// A catalog holds NO parsed documents itself. A query spans several services and several providers,
// so holding documents would make the working set the sum of every document touched instead of the
// exchanges actually needed. A document is parsed, its exchange resolved, and the document released;
// a resolved exchange is self-contained and never refers back to it. Where documents should outlive a
// query — parsing a large one costs more than a request — a DocCache holds them, under a budget it
// enforces, rather than every catalog holding its own.
type catalog struct {
	fsys     fs.FS
	provider aot.Provider

	prefix string // the namespace addresses live under

	docs   DocCache // parsed documents shared across catalogs; nil parses every time
	docKey string

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

func (c *catalog) Operations(addr, verb string) ([]aot.AOTExchange, error) {
	_, svc, res, err := c.split(addr)
	if err != nil {
		return nil, err
	}
	doc, err := c.doc(svc)
	if err != nil {
		return nil, err
	}
	return doc.Verb(res, verb)
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

// split parses an address against THIS catalog's provider. A provider name may contain dots
// (googleapis.com declares itself "google", but another may not), so the provider is matched by
// prefix rather than by counting segments.
func (c *catalog) split(addr string) (provider, service, resource string, err error) {
	full := c.prefix + c.provider.Name()
	rest, ok := strings.CutPrefix(addr, full+".")
	if !ok {
		return "", "", "", fmt.Errorf("stackqldoc: address %q is not for provider %q", addr, full)
	}
	service, resource, ok = strings.Cut(rest, ".")
	if !ok || service == "" || resource == "" {
		return "", "", "", fmt.Errorf("stackqldoc: address %q is not <provider>.<service>.<resource>", addr)
	}
	return full, service, resource, nil
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
	var key string
	if c.docs != nil {
		if fi, err := fs.Stat(c.fsys, file); err == nil {
			key = fmt.Sprintf("%s/%s\x00%d\x00%d", c.docKey, file, fi.Size(), fi.ModTime().UnixNano())
			if d, ok := c.docs.Get(key); ok {
				return d, nil
			}
		}
	}
	b, err := fs.ReadFile(c.fsys, file)
	if err != nil {
		return nil, fmt.Errorf("stackqldoc: read %s: %w", file, err)
	}
	d, err := Parse(b)
	if err != nil {
		return nil, err
	}
	if key != "" {
		c.docs.Put(key, d, int64(len(b))*docCost)
	}
	return d, nil
}

func (c *catalog) servicesLocked() []string {
	out := make([]string, 0, len(c.files))
	for name := range c.files {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
