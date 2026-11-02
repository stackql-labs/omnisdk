package omnisdk

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"reflect"
	"slices"
	"sync"

	"github.com/stackql-labs/omnisdk/internal/sessionstate"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/public/secrets"
)

type ClientSession interface {
	SetProvider(string, ProviderBinding) error
	UnsetProvider(string) error
	SetAuth(string, Auth) error
	PatchAuth(string, map[string]any) error
	UnsetAuth(string, ...string) error
	Snapshot(context.Context, ...string) (Args, error)
}

type clientSession struct {
	mu             sync.Mutex
	state          facade.SessionState
	base           Args
	store          secrets.Store
	policies       facade.RequestPolicy
	id             string
	revision       uint64
	secretRevision uint64
	secretVersions map[string][32]byte
}

func cloneJSON[T any](value T) (T, error) {
	var out T
	raw, err := json.Marshal(value)
	if err != nil {
		return out, fmt.Errorf("session configuration is not valid JSON data")
	}
	err = json.Unmarshal(raw, &out)
	if err != nil {
		return out, fmt.Errorf("invalid session configuration")
	}
	return out, nil
}
func copyClientArgs(args Args) (Args, error) {
	var err error
	args.Params = maps.Clone(args.Params)
	args.TuningByProvider = maps.Clone(args.TuningByProvider)
	args.AuthByProvider, err = cloneJSON(args.AuthByProvider)
	if err != nil {
		return Args{}, err
	}
	args.ProviderAliases, err = cloneJSON(args.ProviderAliases)
	if err != nil {
		return Args{}, err
	}
	args.Inputs, err = bindingSnapshot(args.Inputs)
	if err != nil {
		return Args{}, err
	}
	args.Environment, err = bindingSnapshot(args.Environment)
	if err != nil {
		return Args{}, err
	}
	args.Relations = maps.Clone(args.Relations)
	args.ReadHintsByAlias = maps.Clone(args.ReadHintsByAlias)
	for alias, hints := range args.ReadHintsByAlias {
		hints.Columns = slices.Clone(hints.Columns)
		hints.OrderBy = slices.Clone(hints.OrderBy)
		args.ReadHintsByAlias[alias] = hints
	}
	args.RBAC = slices.Clone(args.RBAC)
	return args, nil
}
func NewClientSession(base Args, store secrets.Store) (ClientSession, error) {
	base, err := copyClientArgs(base)
	if err != nil {
		return nil, err
	}
	base.sharedPolicies = nil
	s := &clientSession{state: sessionstate.New(), base: base, store: store, id: rand.Text(), secretVersions: map[string][32]byte{}}
	for alias, binding := range base.ProviderAliases {
		if err := s.SetProvider(alias, binding); err != nil {
			return nil, err
		}
	}
	s.base.ProviderAliases = nil
	return s, nil
}
func (s *clientSession) SetProvider(alias string, binding ProviderBinding) error {
	if err := validBinding(alias, binding, false); err != nil {
		return err
	}
	raw, err := json.Marshal(binding)
	if err != nil {
		return fmt.Errorf("invalid provider binding")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	bindings := map[string]ProviderBinding{}
	for name, encoded := range s.state.Snapshot() {
		var previous ProviderBinding
		if err := json.Unmarshal(encoded, &previous); err != nil {
			return fmt.Errorf("invalid provider binding")
		}
		bindings[name] = previous
	}
	bindings[alias] = binding
	if err := validateBindingSet(bindings, false); err != nil {
		return err
	}
	s.state.Set(alias, raw)
	s.revision++
	s.policies = nil
	return nil
}
func (s *clientSession) UnsetProvider(alias string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.state.Snapshot(alias)[alias]; !ok {
		return fmt.Errorf("provider alias is not configured")
	}
	s.state.Delete(alias)
	s.revision++
	s.policies = nil
	return nil
}
func (s *clientSession) changeAuth(alias string, change func(*ProviderBinding) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, found := s.state.Snapshot(alias)[alias]
	if !found {
		return fmt.Errorf("provider alias is not configured")
	}
	var binding ProviderBinding
	if err := json.Unmarshal(raw, &binding); err != nil {
		return fmt.Errorf("invalid provider binding")
	}
	if err := change(&binding); err != nil {
		return err
	}
	raw, err := json.Marshal(binding)
	if err != nil {
		return fmt.Errorf("invalid auth configuration")
	}
	s.state.Set(alias, raw)
	s.revision++
	return nil
}
func (s *clientSession) SetAuth(alias string, auth Auth) error {
	return s.changeAuth(alias, func(b *ProviderBinding) error { b.Auth = &auth; b.AuthSecret = ""; return nil })
}
func authField(name string) bool {
	t := reflect.TypeOf(Auth{})
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		tag := field.Tag.Get("json")
		if comma := bytes.IndexByte([]byte(tag), ','); comma >= 0 {
			tag = tag[:comma]
		}
		if tag == "" {
			tag = field.Name
		}
		if name == tag {
			return true
		}
	}
	return false
}
func patchAuth(b *ProviderBinding, fields map[string]any, remove bool) error {
	if b.AuthSecret != "" {
		return fmt.Errorf("replace a secret-backed auth DTO explicitly before editing fields")
	}
	if b.Auth == nil {
		b.Auth = &Auth{}
	}
	raw, _ := json.Marshal(b.Auth)
	var data map[string]json.RawMessage
	if err := json.Unmarshal(raw, &data); err != nil {
		return fmt.Errorf("invalid auth DTO")
	}
	for field, value := range fields {
		if !authField(field) {
			return fmt.Errorf("unknown auth field %q", field)
		}
		if remove {
			delete(data, field)
			continue
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("invalid auth field value")
		}
		data[field] = encoded
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("invalid auth DTO")
	}
	var auth Auth
	if err := json.Unmarshal(raw, &auth); err != nil {
		return fmt.Errorf("auth fields have invalid types")
	}
	b.Auth = &auth
	return nil
}
func (s *clientSession) PatchAuth(alias string, fields map[string]any) error {
	return s.changeAuth(alias, func(b *ProviderBinding) error { return patchAuth(b, fields, false) })
}
func (s *clientSession) UnsetAuth(alias string, fields ...string) error {
	return s.changeAuth(alias, func(b *ProviderBinding) error {
		if len(fields) == 0 {
			b.Auth = nil
			b.AuthSecret = ""
			return nil
		}
		remove := map[string]any{}
		for _, field := range fields {
			remove[field] = nil
		}
		return patchAuth(b, remove, true)
	})
}
func (s *clientSession) Snapshot(ctx context.Context, aliases ...string) (Args, error) {
	if err := ctx.Err(); err != nil {
		return Args{}, err
	}
	s.mu.Lock()
	args, err := copyClientArgs(s.base)
	if err != nil {
		s.mu.Unlock()
		return Args{}, err
	}
	args.ProviderAliases = map[string]ProviderBinding{}
	values := s.state.Snapshot(aliases...)
	for _, alias := range aliases {
		if _, found := values[alias]; !found {
			s.mu.Unlock()
			return Args{}, fmt.Errorf("provider alias %q is not configured", alias)
		}
	}
	for alias, raw := range values {
		var binding ProviderBinding
		if err := json.Unmarshal(raw, &binding); err != nil {
			s.mu.Unlock()
			return Args{}, fmt.Errorf("invalid provider binding")
		}
		args.ProviderAliases[alias] = binding
	}
	if s.policies == nil {
		policyArgs := args
		policyArgs.ProviderAliases = map[string]ProviderBinding{}
		for alias, raw := range s.state.Snapshot() {
			var binding ProviderBinding
			if err := json.Unmarshal(raw, &binding); err != nil {
				s.mu.Unlock()
				return Args{}, fmt.Errorf("invalid provider binding")
			}
			policyArgs.ProviderAliases[alias] = binding
		}
		s.policies = newProviderPolicies(policyArgs)
	}
	args.sharedPolicies = s.policies
	configurationRevision := s.revision
	args.bindingVersion = s.id + "/" + fmt.Sprint(configurationRevision) + "/" + fmt.Sprint(s.secretRevision)
	s.mu.Unlock()
	versions := map[string][32]byte{}
	resolvedSecrets := map[string]Auth{}
	for alias, binding := range args.ProviderAliases {
		if binding.AuthSecret != "" {
			if resolved, found := resolvedSecrets[binding.AuthSecret]; found {
				copied, err := cloneJSON(resolved)
				if err != nil {
					return Args{}, err
				}
				binding.Auth = &copied
				binding.AuthSecret = ""
				args.ProviderAliases[alias] = binding
				continue
			}
			if s.store == nil {
				return Args{}, fmt.Errorf("a secret store is required for provider alias %q", alias)
			}
			raw, err := s.store.Get(ctx, binding.AuthSecret)
			if err != nil {
				return Args{}, fmt.Errorf("cannot resolve auth secret for alias %q: %w", alias, err)
			}
			if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
				return Args{}, fmt.Errorf("auth secret must be an Auth object")
			}
			var auth Auth
			d := json.NewDecoder(bytes.NewReader(raw))
			d.DisallowUnknownFields()
			if err := d.Decode(&auth); err != nil {
				return Args{}, fmt.Errorf("auth secret for alias %q is not a valid Auth DTO", alias)
			}
			if err := d.Decode(new(any)); err != io.EOF {
				return Args{}, fmt.Errorf("auth secret has trailing data")
			}
			versions[binding.AuthSecret] = sha256.Sum256(raw)
			resolvedSecrets[binding.AuthSecret] = auth
			binding.Auth = &auth
			binding.AuthSecret = ""
			args.ProviderAliases[alias] = binding
		}
	}
	if len(versions) > 0 {
		s.mu.Lock()
		changed := false
		for name, version := range versions {
			if previous, found := s.secretVersions[name]; !found || previous != version {
				changed = true
			}
			s.secretVersions[name] = version
		}
		if changed {
			s.secretRevision++
		}
		args.bindingVersion = s.id + "/" + fmt.Sprint(configurationRevision) + "/" + fmt.Sprint(s.secretRevision)
		s.mu.Unlock()
	}
	if err := validateProviderBindings(args); err != nil {
		return Args{}, err
	}
	return args, nil
}
