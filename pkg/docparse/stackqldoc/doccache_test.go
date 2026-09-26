package stackqldoc_test

import (
	"os"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/stackql-labs/omnisdk/pkg/docparse/stackqldoc"
)

const registryRoot = "../../../test/corpus/registry"

// countingCache is a DocCache that records what it was asked.
type countingCache struct {
	mu         sync.Mutex
	docs       map[string]stackqldoc.Doc
	hits, puts int
}

func (c *countingCache) Get(k string) (stackqldoc.Doc, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	d, ok := c.docs[k]
	if ok {
		c.hits++
	}
	return d, ok
}

func (c *countingCache) Put(k string, d stackqldoc.Doc, _ int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.docs[k] = d
	c.puts++
}

func resources(t *testing.T, c stackqldoc.DocCache, root string) {
	t.Helper()
	reg, err := stackqldoc.OpenRegistry(os.DirFS(registryRoot), stackqldoc.WithDocCache(c, root))
	if err != nil {
		t.Fatal(err)
	}
	cat, err := reg.Catalog("stackql_unstable_aws.iam")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cat.Resources("iam"); err != nil {
		t.Fatal(err)
	}
}

// A document is parsed once per view and served from the cache after; another view never sees it.
func TestDocumentsAreCachedPerView(t *testing.T) {
	if _, err := os.Stat(registryRoot); err != nil {
		t.Skip("no corpus")
	}
	c := &countingCache{docs: map[string]stackqldoc.Doc{}}
	resources(t, c, "/view-a")
	resources(t, c, "/view-a")
	if c.puts != 1 || c.hits != 1 {
		t.Errorf("puts=%d hits=%d, want the document parsed once and reused once", c.puts, c.hits)
	}
	resources(t, c, "/view-b")
	if c.puts != 2 || c.hits != 1 {
		t.Errorf("puts=%d hits=%d, want another view parsed apart", c.puts, c.hits)
	}
}

// A provider listing one service twice — a preferred entry with a document and another without —
// resolves to the document whatever the order, and a document the provider names but the bundle
// lacks is named in the error.
func TestServiceEntriesResolveToTheirDocument(t *testing.T) {
	doc := "openapi: 3.0.0\ninfo: {title: s, version: v1}\npaths: {}\n" +
		"components:\n  x-stackQL-resources:\n    r: {id: p.s.r, name: r, methods: {}, sqlVerbs: {select: []}}\n"
	for name, services := range map[string]string{
		"preferred first": "  s:\n    name: s\n    preferred: true\n    service: {$ref: p/v1/services/s-v1.yaml}\n  s2:\n    name: s\n",
		"preferred last":  "  s2:\n    name: s\n  s:\n    name: s\n    preferred: true\n    service: {$ref: p/v1/services/s-v1.yaml}\n",
	} {
		t.Run(name, func(t *testing.T) {
			fsys := fstest.MapFS{
				"provider.yaml":      {Data: []byte("id: p\nname: p\nversion: v1\nproviderServices:\n" + services + "  gone:\n    name: gone\n    service: {$ref: p/v1/services/gone-v1.yaml}\n")},
				"services/s-v1.yaml": {Data: []byte(doc)},
			}
			c, err := stackqldoc.Open(fsys)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.Resources("s"); err != nil {
				t.Errorf("service s: %v", err)
			}
			_, err = c.Resources("gone")
			if err == nil || !strings.Contains(err.Error(), "services/gone-v1.yaml") {
				t.Errorf("missing document: err = %v, want it named", err)
			}
		})
	}
}

// Parameters declared by $ref, and at path level, are the operation's inputs like inline ones.
func TestParametersFollowRefsAndPathLevel(t *testing.T) {
	doc := `openapi: 3.0.0
info: {title: s, version: v1}
servers: [{url: "https://api.example.com"}]
paths:
  /orgs/{org}/members:
    parameters:
      - {name: trace, in: header, required: false}
    get:
      operationId: list_members
      parameters:
        - $ref: '#/components/parameters/org'
        - {name: role, in: query}
components:
  parameters:
    org: {name: org, in: path, required: true}
  x-stackQL-resources:
    members:
      id: p.s.members
      name: members
      methods:
        list_members:
          operation: {$ref: '#/paths/~1orgs~1{org}~1members/get'}
          response: {mediaType: application/json, openAPIDocKey: '200'}
      sqlVerbs:
        select: [{$ref: '#/components/x-stackQL-resources/members/methods/list_members'}]
`
	d, err := stackqldoc.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	ex, err := d.Select("members")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, p := range ex.Request().Parameters() {
		got[p.Name()] = p.In()
		if p.Name() == "org" && !p.Required() {
			t.Error("org lost its required flag through the $ref")
		}
	}
	for name, in := range map[string]string{"org": "path", "role": "query", "trace": "header"} {
		if got[name] != in {
			t.Errorf("parameters = %v, want %s in %s", got, name, in)
		}
	}
}
