package awssts

import (
	"sync"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

type sourceSlot struct {
	fingerprint [32]byte
	source      facade.AWSCredentialSource
}
type sourceCache struct {
	mu      sync.Mutex
	sources map[string]sourceSlot
}

// NewCache creates a session-owned cache. It is never process-global.
func NewCache() facade.AWSCredentialCache { return &sourceCache{sources: map[string]sourceSlot{}} }
func (c *sourceCache) Source(scope string, fingerprint [32]byte, create func() (facade.AWSCredentialSource, error)) (facade.AWSCredentialSource, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if current, ok := c.sources[scope]; ok && current.fingerprint == fingerprint {
		return current.source, nil
	}
	source, err := create()
	if err != nil {
		return nil, err
	}
	c.sources[scope] = sourceSlot{fingerprint: fingerprint, source: source}
	return source, nil
}
