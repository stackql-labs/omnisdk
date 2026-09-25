package stackqldoc_test

import (
	"os"
	"sync"
	"testing"

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
