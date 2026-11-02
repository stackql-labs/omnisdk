package sessionstate

import (
	"maps"
	"slices"
	"sync"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

type memory struct {
	mu     sync.RWMutex
	values map[string][]byte
}

func New() facade.SessionState { return &memory{values: map[string][]byte{}} }
func (m *memory) Set(key string, value []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.values[key] = slices.Clone(value)
}
func (m *memory) Delete(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.values, key)
}
func (m *memory) Snapshot(keys ...string) map[string][]byte {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out map[string][]byte
	if len(keys) > 0 {
		out = map[string][]byte{}
		for _, key := range keys {
			if value, found := m.values[key]; found {
				out[key] = value
			}
		}
	}
	if out == nil {
		out = maps.Clone(m.values)
	}
	for key, value := range out {
		out[key] = slices.Clone(value)
	}
	return out
}
