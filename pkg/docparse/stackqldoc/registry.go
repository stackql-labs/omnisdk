package stackqldoc

import (
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"sync"

	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
)

// OpenRegistry resolves a directory of providers: <root>/<provider>/<version>/provider.yaml plus its
// services/. Providers and versions are DISCOVERED, not configured — the layout already states both,
// and a second source of truth about which version is current is a way to disagree with the disk.
//
// Nothing is parsed here. A registry can hold thousands of resources across dozens of providers, so
// opening one reads directory names only; a provider's documents are touched when something addresses
// them, and dropped again after (see catalog).
func OpenRegistry(fsys fs.FS, opts ...Option) (aot.Registry, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("stackqldoc: open registry: %w", err)
	}
	r := &registry{
		fsys: fsys, versions: map[string]string{}, dirs: map[string]string{},
		catalogs: map[string]aot.Catalog{}, opts: opts, prefix: resolveSettings(opts).prefix,
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		v, ok := latestVersion(fsys, e.Name())
		if !ok {
			continue
		}
		r.versions[e.Name()] = v
		// A provider is addressed by the name it DECLARES, which is not always its directory:
		// googleapis.com/ declares itself "google", and its resources' own ids say google too.
		// Addressing by directory would name something the documents never call it.
		name := e.Name()
		if b, err := fs.ReadFile(fsys, e.Name()+"/"+v+"/provider.yaml"); err == nil {
			if p, err := ParseProvider(b); err == nil && p.Name() != "" {
				name = p.Name()
			}
		}
		r.dirs[name] = e.Name()
	}
	if len(r.versions) == 0 {
		return nil, fmt.Errorf("stackqldoc: no providers under registry root " +
			"(expected <provider>/<version>/provider.yaml)")
	}
	return r, nil
}

// latestVersion picks the newest version directory holding a provider.yaml. stackql versions sort
// lexically (v26.05.00393), so newest is the maximum; a directory without a provider.yaml is not a
// version, which is what keeps stray directories from being mistaken for one.
func latestVersion(fsys fs.FS, provider string) (string, bool) {
	entries, err := fs.ReadDir(fsys, provider)
	if err != nil {
		return "", false
	}
	var versions []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := fs.Stat(fsys, provider+"/"+e.Name()+"/provider.yaml"); err == nil {
			versions = append(versions, e.Name())
		}
	}
	if len(versions) == 0 {
		return "", false
	}
	sort.Strings(versions)
	return versions[len(versions)-1], true
}

type registry struct {
	fsys     fs.FS
	versions map[string]string // directory → version
	dirs     map[string]string // declared provider name → directory
	opts     []Option
	prefix   string

	mu       sync.Mutex
	catalogs map[string]aot.Catalog // one per provider; holds no documents, only the file index
}

// Providers reports the namespaced names, since those are what addresses use. Nothing outside this
// package should have to know a directory is called "aws" while its addresses say
// "stackql_preview_aws".
func (r *registry) Providers() []string {
	out := make([]string, 0, len(r.dirs))
	for name := range r.dirs {
		out = append(out, r.prefix+name)
	}
	sort.Strings(out)
	return out
}

func (r *registry) Version(provider string) (string, bool) {
	dir, ok := r.dirs[r.dirName(provider)]
	if !ok {
		return "", false
	}
	v, ok := r.versions[dir]
	return v, ok
}

// providerOf finds which provider an address names. A provider name may itself contain dots
// (googleapis.com), so the FIRST dot is not the boundary — the longest declared name that the address
// starts with is.
func (r *registry) providerOf(address string) (string, bool) {
	best := ""
	for name := range r.dirs {
		full := r.prefix + name
		if strings.HasPrefix(address, full+".") && len(full) > len(best) {
			best = full
		}
	}
	return best, best != ""
}

// dirName maps a namespaced provider back to its declared name. Accepting the bare name too means a
// caller browsing the filesystem is not punished for it.
func (r *registry) dirName(provider string) string {
	return strings.TrimPrefix(provider, r.prefix)
}

func (r *registry) Catalog(provider string) (aot.Catalog, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.catalogs[r.dirName(provider)]; ok {
		return c, nil
	}
	name := r.dirName(provider)
	dir, ok := r.dirs[name]
	if !ok {
		// Callers reach for a catalog by provider name, but often hold a full address; resolving one
		// from the other here saves every caller re-deriving where the provider name ends.
		if full, found := r.providerOf(provider); found {
			name = r.dirName(full)
			dir, ok = r.dirs[name]
		}
	}
	if !ok {
		return nil, fmt.Errorf("stackqldoc: no provider %q in registry (have %d: %v)",
			provider, len(r.dirs), r.Providers())
	}
	version := r.versions[dir]
	sub, err := fs.Sub(r.fsys, dir+"/"+version)
	if err != nil {
		return nil, err
	}
	c, err := Open(sub, r.opts...)
	if err != nil {
		return nil, err
	}
	// A catalog is safe to keep: it holds the provider document and a file index, never a service
	// document. That is what makes handing over a whole registry root affordable.
	r.catalogs[r.dirName(provider)] = c
	return c, nil
}

func (r *registry) Exchange(address string) (aot.AOTExchange, error) {
	provider, ok := r.providerOf(address)
	if !ok {
		return nil, fmt.Errorf("stackqldoc: address %q names no provider in this registry (have %v)",
			address, r.Providers())
	}
	c, err := r.Catalog(provider)
	if err != nil {
		return nil, err
	}
	return c.Exchange(address)
}
