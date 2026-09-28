package cache_test

import (
	"testing"
	"time"

	"github.com/stackql-labs/omnisdk/pkg/cache"
)

func TestEvictsLeastRecentlyUsedByCost(t *testing.T) {
	c := cache.New[string, int](cache.Config{MaxCost: 10})
	c.Put("a", 1, 4)
	c.Put("b", 2, 4)
	c.Get("a") // b is now least recently used
	c.Put("c", 3, 4)
	if _, ok := c.Get("b"); ok {
		t.Error("b survived though least recently used")
	}
	if _, ok := c.Get("a"); !ok {
		t.Error("a evicted though recently used")
	}
	if c.Cost() != 8 {
		t.Errorf("cost = %d, want 8", c.Cost())
	}
}

func TestOverBudgetValueIsNotStored(t *testing.T) {
	c := cache.New[string, int](cache.Config{MaxCost: 10})
	c.Put("big", 1, 11)
	if c.Len() != 0 {
		t.Error("stored a value costing more than the budget")
	}
}

func TestMaxEntriesAndTTL(t *testing.T) {
	c := cache.New[string, int](cache.Config{MaxCost: 100, MaxEntries: 1})
	c.Put("a", 1, 1)
	c.Put("b", 2, 1)
	if c.Len() != 1 {
		t.Errorf("len = %d, want 1", c.Len())
	}
	e := cache.New[string, int](cache.Config{MaxCost: 100, TTL: time.Millisecond})
	e.Put("a", 1, 1)
	time.Sleep(5 * time.Millisecond)
	if _, ok := e.Get("a"); ok {
		t.Error("expired entry returned")
	}
}

func TestAutomaticBudgetIsPositive(t *testing.T) {
	if b := (cache.Config{}).Budget(); b <= 0 {
		t.Errorf("budget = %d", b)
	}
	if b := (cache.Config{MaxCost: 7}).Budget(); b != 7 {
		t.Errorf("stated budget = %d, want 7", b)
	}
}
