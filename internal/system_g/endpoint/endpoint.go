// Package endpoint resolves a logical SERVICE to the URL its requests go to. Every provider host in
// the codebase is reached through here, so retargeting a run at a mock is a config decision rather
// than a code path.
//
// Two things make that work.
//
// A service's real URL is REGISTERED at exchange init time, not written inline at each call site, so
// there is exactly one statement of where a service lives. Templates carry {vars} (e.g. {region})
// expanded per request.
//
// An override replaces either the WHOLE url or FRAGMENTS of it. Whole is for a mock whose layout
// differs from the provider's (S3's virtual-host addressing collapses to path-style against a single
// local host). Fragments — scheme/host/port/path — are for a mock that mirrors the provider's paths
// and only lives somewhere else, which is the common case and the one that keeps each service's real
// path shape without restating it in the override.
//
// Resolution is by CONSTRUCTION, never mutation: an exchange is built per bind-join inner under
// concurrent fan-out, so there is no long-lived object to retarget after the fact.
package endpoint

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
)

// The services every provider host is reached through. A new exchange talking to a new host adds a
// constant AND a Register call, which is what keeps the set enumerable — and therefore assertable by
// an integration gate checking a mock covers everything a plan can reach.
const (
	AWSS3      = "aws.s3"
	AWSEC2     = "aws.ec2"
	AWSIAM     = "aws.iam"
	AzureLogin = "azure.login"
	AzureMgmt  = "azure.mgmt"
	AzureGraph = "azure.graph"
	GCPOAuth   = "gcp.oauth"
	GCPStorage = "gcp.storage"
	GCPCRM     = "gcp.crm"
	GCPCompute = "gcp.compute"
)

var (
	mu       sync.RWMutex
	defaults = map[string]string{}
)

// Register declares a service's real URL template, at exchange init time. Templates may carry {vars}
// (e.g. https://s3.{region}.amazonaws.com) expanded at resolve time. Registering twice panics: two
// statements of where one service lives is the drift this package exists to prevent.
func Register(service, template string) {
	mu.Lock()
	defer mu.Unlock()
	if prev, dup := defaults[service]; dup {
		panic(fmt.Sprintf("endpoint: service %q already registered as %q", service, prev))
	}
	defaults[service] = template
}

// Default is a service's registered real URL template, or "" if unregistered.
func Default(service string) string {
	mu.RLock()
	defer mu.RUnlock()
	return defaults[service]
}

// All is every registered service, sorted — the surface an integration gate walks.
func All() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(defaults))
	for k := range defaults {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Endpoints resolves a service to the URL to use for it. Impls are unexported and immutable; build
// one with Real, Whole, Fragment, Mapped or Parse.
type Endpoints interface {
	// Resolve returns the URL for service with vars expanded, after any override.
	Resolve(service string, vars map[string]string) string
	// IsOverridden reports whether this service is redirected. A caller whose real addressing differs
	// from a mock's (S3 virtual-host vs path-style) branches on this rather than sniffing the URL.
	IsOverridden(service string) bool
	// Overridden lists the redirected services, sorted. Empty means everything is real.
	Overridden() []string
}

// Real is the clouds: every service resolves to its registered default.
func Real() Endpoints { return mapped{} }

// Whole replaces a service's entire URL, ignoring the registered path shape.
func Whole(rawURL string) Override { return override{whole: strings.TrimRight(rawURL, "/")} }

// Fragment replaces only the named parts of a service's registered URL; empty fields are left alone.
// Path, when set, replaces the default's path — use it to relocate under a prefix, not to erase one.
func Fragment(scheme, host, port, path string) Override {
	return override{frag: &fragment{Scheme: scheme, Host: host, Port: port, Path: path}}
}

// Override is one service's redirect: a whole URL or a set of fragments. Impls are unexported.
type Override interface {
	apply(defaultURL string) string
}

// Uniform sends EVERY registered service to one host, replacing scheme/host/port and KEEPING each
// service's registered path. This is what a bare --endpoint means: the mock mirrors the providers'
// paths and merely lives elsewhere.
func Uniform(rawURL string) Endpoints {
	if strings.TrimSpace(rawURL) == "" {
		return mapped{}
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return mapped{}
	}
	return uniform{scheme: u.Scheme, host: u.Hostname(), port: u.Port(), prefix: strings.TrimRight(u.Path, "/")}
}

// Mapped redirects the named services and leaves the rest real, so a run can mock one provider while
// genuinely talking to another.
func Mapped(m map[string]Override) Endpoints {
	out := make(mapped, len(m))
	for k, v := range m {
		if v != nil {
			out[k] = v
		}
	}
	return out
}

// Parse reads the wire form a consumer supplies:
//
//	""                                    the real clouds
//	"http://host:8085"                    every service at that host, each keeping its own path
//	{"aws.s3": "http://host:8085/s3"}     that service's WHOLE url replaced
//	{"aws.s3": {"host":"h","port":"1"}}   that service's url with only those FRAGMENTS replaced
//
// An unknown service key is an error: ignoring it would leave a run the caller believes is mocked
// pointed at the real cloud.
func Parse(spec string) (Endpoints, error) {
	s := strings.TrimSpace(spec)
	if s == "" {
		return Real(), nil
	}
	if !strings.HasPrefix(s, "{") {
		return Uniform(s), nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return nil, fmt.Errorf("endpoint: parse per-service map: %w", err)
	}
	known := map[string]bool{}
	for _, svc := range All() {
		known[svc] = true
	}
	out := map[string]Override{}
	for k, v := range raw {
		if !known[k] {
			return nil, fmt.Errorf("endpoint: unknown service %q (known: %s)", k, strings.Join(All(), ", "))
		}
		var whole string
		if err := json.Unmarshal(v, &whole); err == nil {
			out[k] = Whole(whole)
			continue
		}
		var f fragment
		if err := json.Unmarshal(v, &f); err != nil {
			return nil, fmt.Errorf("endpoint: service %q: want a url string or {scheme,host,port,path}: %w", k, err)
		}
		out[k] = override{frag: &f}
	}
	return Mapped(out), nil
}

// fragment is the parts of a URL an override may replace; empty means "keep the default's".
type fragment struct {
	Scheme string `json:"scheme,omitempty"`
	Host   string `json:"host,omitempty"`
	Port   string `json:"port,omitempty"`
	Path   string `json:"path,omitempty"`
}

type override struct {
	whole string
	frag  *fragment
}

func (o override) apply(defaultURL string) string {
	if o.frag == nil {
		return o.whole
	}
	return o.frag.apply(defaultURL)
}

func (f fragment) apply(defaultURL string) string {
	u, err := url.Parse(defaultURL)
	if err != nil {
		return defaultURL
	}
	if f.Scheme != "" {
		u.Scheme = f.Scheme
	}
	if f.Host != "" || f.Port != "" {
		host, port := u.Hostname(), u.Port()
		if f.Host != "" {
			host = f.Host
		}
		if f.Port != "" {
			port = f.Port
		}
		u.Host = host
		if port != "" {
			u.Host = host + ":" + port
		}
	}
	if f.Path != "" {
		u.Path = "/" + strings.Trim(f.Path, "/")
	}
	return strings.TrimRight(u.String(), "/")
}

// uniform relocates every service to one host while preserving each service's registered path.
type uniform struct{ scheme, host, port, prefix string }

func (u uniform) Resolve(service string, vars map[string]string) string {
	def := expand(Default(service), vars)
	moved := fragment{Scheme: u.scheme, Host: u.host, Port: u.port}.apply(def)
	if u.prefix == "" {
		return moved
	}
	// A mock mounted under a prefix keeps the service's own path beneath it.
	p, err := url.Parse(moved)
	if err != nil {
		return moved
	}
	p.Path = u.prefix + p.Path
	return strings.TrimRight(p.String(), "/")
}

func (u uniform) IsOverridden(string) bool { return true }
func (u uniform) Overridden() []string     { return All() }

// mapped redirects only the services it names; the empty map is Real.
type mapped map[string]Override

func (m mapped) Resolve(service string, vars map[string]string) string {
	def := expand(Default(service), vars)
	if o, ok := m[service]; ok {
		return expand(o.apply(def), vars)
	}
	return def
}

func (m mapped) IsOverridden(service string) bool {
	_, ok := m[service]
	return ok
}

func (m mapped) Overridden() []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// expand substitutes {name} placeholders from vars.
func expand(tmpl string, vars map[string]string) string {
	if tmpl == "" || len(vars) == 0 || !strings.Contains(tmpl, "{") {
		return tmpl
	}
	out := tmpl
	for k, v := range vars {
		out = strings.ReplaceAll(out, "{"+k+"}", v)
	}
	return out
}
