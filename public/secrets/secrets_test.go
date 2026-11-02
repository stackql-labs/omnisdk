package secrets_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stackql-labs/omnisdk/public/secrets"
)

func TestFileStorePersistenceIsolationAndConcurrency(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "secrets.json")
	ctx := context.Background()
	first, err := secrets.New(secrets.Config{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	second, err := secrets.New(secrets.Config{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Get(ctx, "absent"); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			store := first
			if i%2 == 0 {
				store = second
			}
			if err := store.Set(ctx, fmt.Sprint(i), []byte{0, byte(i), 255}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	for i := range 20 {
		value, err := second.Get(ctx, fmt.Sprint(i))
		if err != nil || len(value) != 3 || value[1] != byte(i) {
			t.Fatalf("value=%v err=%v", value, err)
		}
		value[1] = 200
	}
	if err := first.Delete(ctx, "1"); err != nil {
		t.Fatal(err)
	}
	if _, err := second.Get(ctx, "1"); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(path)
		if info.Mode().Perm() != 0600 {
			t.Fatal(info.Mode())
		}
	}
}
func TestFileStoreFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store")
	ctx := context.Background()
	store, err := secrets.New(secrets.Config{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"valid":"eA=="} trailing`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, "valid"); err == nil {
		t.Fatal("accepted corrupt data")
	}
	if err := store.Set(ctx, "new", []byte("secret")); err == nil {
		t.Fatal("overwrote corrupt file")
	}
	if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		os.Chmod(path, 0644)
		if err := store.Set(ctx, "new", []byte("secret")); err == nil {
			t.Fatal("accepted public file permissions")
		}
		os.Chmod(path, 0600)
	}
	if err := os.WriteFile(path+".lock", nil, 0600); err != nil {
		t.Fatal(err)
	}
	deadline, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if err := store.Set(deadline, "new", []byte("secret")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if _, err := secrets.New(secrets.Config{}); err == nil {
		t.Fatal("inferred a secret path")
	}
	if _, err := secrets.New(secrets.Config{Backend: "unknown"}); err == nil {
		t.Fatal("accepted unknown backend")
	}
}
