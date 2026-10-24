package omnisdk

// ProviderAuth is a serialised provider credential declaration, including fallbacks.
type ProviderAuth struct {
	AuthConfig `yaml:",inline"`
	Successor  *ProviderAuth `json:"successor,omitempty" yaml:"successor,omitempty"`
}

// AuthMetadata is the presentation DTO returned by a host's SHOW AUTH command.
type AuthMetadata struct {
	Principal string `json:"principal"`
	Type      string `json:"type"`
	Source    string `json:"source"`
}

func (a *AuthMetadata) ToMap() map[string]interface{} {
	return map[string]interface{}{"principal": a.Principal, "type": a.Type, "source": a.Source}
}

// ProviderDescription is a registry index entry.
type ProviderDescription struct {
	Versions []string `json:"versions" yaml:"versions"`
}
