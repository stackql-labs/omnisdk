package rowcache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func testKey() Key {
	return Key{Partition: "tenant-a", Resource: "p.s.items", Method: "list", Document: "doc-v1", Request: "request-v1"}
}
func memoryCache(t *testing.T, cfg Config, limits MemoryConfig) Managed {
	t.Helper()
	c, err := NewMemory(context.Background(), cfg, limits)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}
func put(t *testing.T, c Cache, k Key, row map[string]any) {
	t.Helper()
	w, err := c.Begin(context.Background(), k)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Abort(context.Background())
	if row != nil {
		if err = w.Append(context.Background(), row); err != nil {
			t.Fatal(err)
		}
	}
	if err = w.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func TestPublicationIsolationAndEmptyHits(t *testing.T) {
	c := memoryCache(t, Config{}, MemoryConfig{})
	ctx := context.Background()
	key := testKey()
	source := map[string]any{"nested": map[string]any{"id": json.Number("9007199254740993")}, "bytes": []byte{1, 2}}
	w, err := c.Begin(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if err = w.Append(ctx, source); err != nil {
		t.Fatal(err)
	}
	if _, hit, _ := c.Lookup(ctx, key); hit {
		t.Fatal("partial entry visible")
	}
	source["nested"].(map[string]any)["id"] = "mutated"
	if err = w.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		rows, hit, err := c.Lookup(ctx, key)
		if err != nil || !hit {
			t.Fatalf("hit=%v err=%v", hit, err)
		}
		if !rows.Next(ctx) {
			t.Fatal("missing row")
		}
		row := rows.Row()
		if row["nested"].(map[string]any)["id"] != json.Number("9007199254740993") {
			t.Fatal(row)
		}
		row["nested"].(map[string]any)["id"] = "changed by reader"
		row["bytes"].([]byte)[0] = 9
		if rows.Next(ctx) || rows.Err() != nil {
			t.Fatal("bad tail")
		}
		rows.Close()
	}
	empty := key
	empty.Request = "empty"
	put(t, c, empty, nil)
	rows, hit, err := c.Lookup(ctx, empty)
	if err != nil || !hit || rows.Next(ctx) {
		t.Fatalf("empty hit=%v err=%v", hit, err)
	}
	rows.Close()
}
func TestTTLStartsAtCommitAndGCIsIndependent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, err := NewMemory(context.Background(), Config{TTL: time.Second, GCInterval: time.Second}, MemoryConfig{})
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		ctx := context.Background()
		key := testKey()
		w, err := c.Begin(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if err = w.Append(ctx, map[string]any{"id": 1}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Second)
		synctest.Wait()
		if err = w.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		rows, hit, err := c.Lookup(ctx, key)
		if err != nil || !hit {
			t.Fatal("TTL started before commit")
		}
		rows.Close()
		time.Sleep(2 * time.Second)
		synctest.Wait()
		s := c.(*managed).store.(*memory)
		s.mu.Lock()
		entries, heapSize, bytes := len(s.entries), len(s.expiry), s.bytes
		s.mu.Unlock()
		if entries != 0 || heapSize != 0 || bytes != 0 {
			t.Fatalf("GC retained entries=%d heap=%d bytes=%d", entries, heapSize, bytes)
		}
	})
}
func TestExpiredLookupWithoutCollectorAndAbortPreservesOldEntry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, err := NewMemory(context.Background(), Config{TTL: time.Second, GCInterval: time.Hour}, MemoryConfig{})
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		key := testKey()
		put(t, c, key, map[string]any{"id": "old"})
		w, err := c.Begin(context.Background(), key)
		if err != nil {
			t.Fatal(err)
		}
		if err = w.Append(context.Background(), map[string]any{"id": "replacement"}); err != nil {
			t.Fatal(err)
		}
		if err = w.Abort(context.Background()); err != nil {
			t.Fatal(err)
		}
		rows, hit, _ := c.Lookup(context.Background(), key)
		if !hit || !rows.Next(context.Background()) || rows.Row()["id"] != "old" {
			t.Fatal("abort replaced old entry")
		}
		rows.Close()
		time.Sleep(2 * time.Second)
		if _, hit, _ = c.Lookup(context.Background(), key); hit {
			t.Fatal("expired entry served before GC")
		}
	})
}
func TestBudgetsCancellationAndBoundedExpiryHeap(t *testing.T) {
	c := memoryCache(t, Config{}, MemoryConfig{MaxBytes: 2048, MaxEntryBytes: 1024, MaxEntries: 2})
	ctx := context.Background()
	key := testKey()
	for i := 0; i < 100; i++ {
		put(t, c, key, map[string]any{"id": i})
	}
	s := c.(*managed).store.(*memory)
	s.mu.Lock()
	if len(s.expiry) != 1 {
		t.Fatal("stale expiry records accumulated")
	}
	s.mu.Unlock()
	w, err := c.Begin(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if err = w.Append(ctx, map[string]any{"large": make([]byte, 4096)}); !errors.Is(err, ErrCapacity) {
		t.Fatal(err)
	}
	w.Abort(ctx)
	w, err = c.Begin(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err = w.Commit(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	w.Abort(ctx)
	cycle := map[string]any{}
	cycle["self"] = cycle
	w, err = c.Begin(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if err = w.Append(ctx, cycle); err == nil {
		t.Fatal("accepted cyclic row")
	}
	w.Abort(ctx)
	s.mu.Lock()
	reserved, writers := s.reserved, s.writers
	s.mu.Unlock()
	if reserved != 0 || writers != 0 {
		t.Fatalf("leaked pending writes %d/%d", reserved, writers)
	}
}
func TestPartitionInputsAndConcurrentAccess(t *testing.T) {
	c := memoryCache(t, Config{}, MemoryConfig{})
	key := testKey()
	put(t, c, key, map[string]any{"id": 1})
	for _, other := range []Key{
		{Partition: "tenant-b", Resource: key.Resource, Method: key.Method, Document: key.Document, Request: key.Request},
		{Partition: key.Partition, Resource: key.Resource, Method: key.Method, Document: "doc-v2", Request: key.Request},
		{Partition: key.Partition, Resource: key.Resource, Method: key.Method, Document: key.Document, Request: key.Request, Inputs: map[string]any{"region": "elsewhere"}},
	} {
		if _, hit, _ := c.Lookup(context.Background(), other); hit {
			t.Fatal("cross-key hit")
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Go(func() {
			for j := 0; j < 20; j++ {
				k := testKey()
				k.Request = fmt.Sprint(j)
				w, err := c.Begin(context.Background(), k)
				if err != nil {
					t.Error(err)
					return
				}
				if err = w.Append(context.Background(), map[string]any{"id": j}); err != nil {
					t.Error(err)
				}
				if err = w.Commit(context.Background()); err != nil {
					t.Error(err)
				}
				rows, hit, err := c.Lookup(context.Background(), k)
				if err != nil || !hit {
					t.Errorf("hit=%v err=%v", hit, err)
					return
				}
				for rows.Next(context.Background()) {
				}
				if err = rows.Err(); err != nil {
					t.Error(err)
				}
				rows.Close()
			}
		})
	}
	wg.Wait()
}
func TestLifecycleAndExplicitPartition(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	c, err := NewMemory(ctx, Config{}, MemoryConfig{})
	if err != nil {
		t.Fatal(err)
	}
	key := testKey()
	key.Partition = ""
	if _, _, err = c.Lookup(ctx, key); err == nil {
		t.Fatal("missing partition accepted")
	}
	cancel()
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err = c.Lookup(context.Background(), testKey()); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}

func TestLRUEvictionPreservesOpenSnapshot(t *testing.T) {
	c := memoryCache(t, Config{}, MemoryConfig{MaxEntries: 2})
	a, b, d := testKey(), testKey(), testKey()
	b.Request = "b"
	d.Request = "d"
	put(t, c, a, map[string]any{"id": "a"})
	put(t, c, b, map[string]any{"id": "b"})
	snapshot, hit, err := c.Lookup(context.Background(), b)
	if err != nil || !hit {
		t.Fatal(err)
	}
	defer snapshot.Close()
	recent, hit, _ := c.Lookup(context.Background(), a)
	if !hit {
		t.Fatal("missing a")
	}
	recent.Close()
	put(t, c, d, map[string]any{"id": "d"})
	if _, hit, _ := c.Lookup(context.Background(), b); hit {
		t.Fatal("least recently used entry retained")
	}
	if !snapshot.Next(context.Background()) || snapshot.Row()["id"] != "b" {
		t.Fatal("eviction invalidated reader snapshot")
	}
}
