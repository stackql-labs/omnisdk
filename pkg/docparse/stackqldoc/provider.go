package stackqldoc

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
)

// ParseProvider reads a provider document — the level ABOVE a service document. It lists the services
// on offer and declares, once, the signing algorithm every call to them inherits.
func ParseProvider(b []byte) (aot.Provider, error) {
	var raw providerDoc
	if err := yaml.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("stackqldoc: parse provider: %w", err)
	}
	if raw.Name == "" && len(raw.Services) == 0 {
		return nil, fmt.Errorf("stackqldoc: not a provider document (no name, no providerServices)")
	}
	return &provider{doc: raw}, nil
}

// providerDoc is the wire shape; provider is what satisfies the contract. They are separate types
// because the document's field names are the same words as the accessor names.
type providerDoc struct {
	ID       string                     `yaml:"id"`
	Name     string                     `yaml:"name"`
	Version  string                     `yaml:"version"`
	Services map[string]providerService `yaml:"providerServices"`
	Config   struct {
		Auth struct {
			Type              string `yaml:"type"`
			CredentialsEnvVar string `yaml:"credentialsenvvar"`
			KeyIDEnvVar       string `yaml:"keyIDenvvar"`
		} `yaml:"auth"`
	} `yaml:"config"`
}

type providerService struct {
	ID      string `yaml:"id"`
	Name    string `yaml:"name"`
	Title   string `yaml:"title"`
	Version string `yaml:"version"`
	Service struct {
		Ref string `yaml:"$ref"`
	} `yaml:"service"`
}

type provider struct{ doc providerDoc }

func (p *provider) ID() string      { return p.doc.ID }
func (p *provider) Name() string    { return p.doc.Name }
func (p *provider) Version() string { return p.doc.Version }

func (p *provider) Services() []aot.Service {
	out := make([]aot.Service, 0, len(p.doc.Services))
	for key, s := range p.doc.Services {
		if s.Name == "" {
			s.Name = key // the map key is the service's name when the entry omits it
		}
		out = append(out, service{s})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

func (p *provider) Service(name string) (aot.Service, bool) {
	s, ok := p.doc.Services[name]
	if !ok {
		return nil, false
	}
	if s.Name == "" {
		s.Name = name
	}
	return service{s}, true
}

// Security normalizes the provider's declared auth type. The document says aws_signing_v4; what that
// means to a signer is this package's business, not a caller's.
func (p *provider) Security() aot.Security {
	t := p.doc.Config.Auth.Type
	switch strings.ToLower(t) {
	case "aws_signing_v4":
		return security{scheme: aot.SchemeAWSSigV4, name: t}
	default:
		return security{name: t}
	}
}

func (p *provider) Credentials() aot.CredentialSource {
	return credentials{keyID: p.doc.Config.Auth.KeyIDEnvVar, secret: p.doc.Config.Auth.CredentialsEnvVar}
}

type service struct{ s providerService }

func (s service) Name() string    { return s.s.Name }
func (s service) Title() string   { return s.s.Title }
func (s service) Version() string { return s.s.Version }
func (s service) Ref() string     { return s.s.Service.Ref }

type credentials struct{ keyID, secret string }

func (c credentials) KeyIDEnvVar() string  { return c.keyID }
func (c credentials) SecretEnvVar() string { return c.secret }
