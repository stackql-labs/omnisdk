package secretstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

type fileStore struct {
	path string
	mu   sync.Mutex
}

func NewFile(path string) (facade.SecretStore, error) {
	if path == "" {
		return nil, errors.New("secret store path is required")
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	return &fileStore{path: path}, nil
}

func validKey(key string) error {
	if key == "" {
		return errors.New("secret name is required")
	}
	return nil
}

func (s *fileStore) read() (map[string][]byte, error) {
	info, err := os.Lstat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string][]byte{}, nil
	}
	if err != nil {
		return nil, err
	}
	if info.Size() > 64<<20 {
		return nil, errors.New("secret store exceeds 64 MiB")
	}
	if !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0) {
		return nil, errors.New("secret store must be a private regular file (0600)")
	}
	f, err := os.Open(s.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var values map[string][]byte
	decoder := json.NewDecoder(io.LimitReader(f, 64<<20))
	if err := decoder.Decode(&values); err != nil {
		return nil, errors.New("invalid secret store file")
	}
	if values == nil {
		return nil, errors.New("invalid secret store file")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("invalid secret store trailing data")
	}
	return values, nil
}

func (s *fileStore) Get(ctx context.Context, key string) ([]byte, error) {
	if err := validKey(key); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := s.lock(ctx); err != nil {
		return nil, err
	}
	defer s.mu.Unlock()
	values, err := s.read()
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	value, found := values[key]
	if !found {
		return nil, ErrNotFound
	}
	return value, nil
}

func (s *fileStore) update(ctx context.Context, key string, value []byte, remove bool) error {
	if err := validKey(key); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.lock(ctx); err != nil {
		return err
	}
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	// An exclusive sidecar serializes other processes; stale locks require explicit operator cleanup.
	lockCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	lockPath := s.path + ".lock"
	for {
		lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err == nil {
			lock.Close()
			break
		}
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		select {
		case <-lockCtx.Done():
			return lockCtx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	defer os.Remove(lockPath)
	values, err := s.read()
	if err != nil {
		return err
	}
	if remove {
		if _, found := values[key]; !found {
			return ErrNotFound
		}
		delete(values, key)
	} else {
		values[key] = value
	}
	data, err := json.Marshal(values)
	if err != nil {
		return errors.New("cannot encode secret store")
	}
	if len(data) >= 64<<20 {
		return errors.New("secret store exceeds 64 MiB")
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), ".secrets-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), s.path); err != nil {
		return fmt.Errorf("replace secret store: %w", err)
	}
	return syncDirectory(filepath.Dir(s.path))
}
func (s *fileStore) Set(ctx context.Context, key string, value []byte) error {
	return s.update(ctx, key, value, false)
}
func (s *fileStore) Delete(ctx context.Context, key string) error {
	return s.update(ctx, key, nil, true)
}

func (s *fileStore) lock(ctx context.Context) error {
	for !s.mu.TryLock() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
	if err := ctx.Err(); err != nil {
		s.mu.Unlock()
		return err
	}
	return nil
}
