// Package principal defines verified identities and immutable directory snapshots.
package principal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

type Identity = facade.Identity
type Principal = facade.Principal
type Directory = facade.PrincipalDirectory
type identity struct{ issuer, subject string }

func (i identity) Issuer() string  { return i.issuer }
func (i identity) Subject() string { return i.subject }

// NewIdentity describes an identity; only authenticators may establish its authenticity.
func NewIdentity(issuer, subject string) (Identity, error) {
	if issuer == "" || subject == "" {
		return nil, errors.New("identity issuer and subject are required")
	}
	return identity{issuer, subject}, nil
}

type entry struct {
	id         string
	groups     []string
	attributes map[string]string
}

func (e entry) ID() string                    { return e.id }
func (e entry) Groups() []string              { return slices.Clone(e.groups) }
func (e entry) Attributes() map[string]string { return maps.Clone(e.attributes) }

// NewPrincipal builds a snapshot for application-owned directories.
func NewPrincipal(id string, groups []string, attributes map[string]string) (Principal, error) {
	if id == "" {
		return nil, errors.New("principal id is required")
	}
	seen := map[string]bool{}
	for _, g := range groups {
		if g == "" || seen[g] {
			return nil, errors.New("empty or duplicate group")
		}
		seen[g] = true
	}
	return entry{id, slices.Clone(groups), maps.Clone(attributes)}, nil
}

// Config is the directory's JSON format. Groups must be declared explicitly.
type Config struct {
	Groups     []string `json:"groups"`
	Principals []Record `json:"principals"`
}
type Record struct {
	ID         string            `json:"id"`
	Identities []IdentityRecord  `json:"identities"`
	Groups     []string          `json:"groups"`
	Attributes map[string]string `json:"attributes,omitempty"`
}
type IdentityRecord struct {
	Issuer  string `json:"issuer"`
	Subject string `json:"subject"`
}
type memory struct{ entries map[identity]Principal }

func (m memory) Resolve(ctx context.Context, i Identity) (Principal, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if i == nil {
		return nil, errors.New("verified identity is required")
	}
	p, ok := m.entries[identity{i.Issuer(), i.Subject()}]
	if !ok {
		return nil, errors.New("identity is not in the directory")
	}
	return p, nil
}
func NewMemoryDirectoryFromBytes(data []byte) (Directory, error) {
	var cfg Config
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("directory: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return nil, errors.New("directory: trailing data")
	}
	groups := map[string]bool{}
	for _, g := range cfg.Groups {
		if g == "" || groups[g] {
			return nil, errors.New("directory: empty or duplicate group")
		}
		groups[g] = true
	}
	m := memory{map[identity]Principal{}}
	ids := map[string]bool{}
	for _, r := range cfg.Principals {
		if ids[r.ID] || len(r.Identities) == 0 {
			return nil, errors.New("directory: duplicate principal or missing identities")
		}
		ids[r.ID] = true
		for _, g := range r.Groups {
			if !groups[g] {
				return nil, fmt.Errorf("directory: undeclared group %q", g)
			}
		}
		p, err := NewPrincipal(r.ID, r.Groups, r.Attributes)
		if err != nil {
			return nil, err
		}
		for _, i := range r.Identities {
			v, err := NewIdentity(i.Issuer, i.Subject)
			if err != nil {
				return nil, err
			}
			key := identity{v.Issuer(), v.Subject()}
			if _, ok := m.entries[key]; ok {
				return nil, errors.New("directory: ambiguous identity")
			}
			m.entries[key] = p
		}
	}
	return m, nil
}
