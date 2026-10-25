package omnisdk

import "gopkg.in/yaml.v3"

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

// UnmarshalYAML accepts provider document credential names alongside runtime names.
func (a *ProviderAuth) UnmarshalYAML(node *yaml.Node) error {
	type declaration ProviderAuth
	var decoded declaration
	if err := node.Decode(&decoded); err != nil {
		return err
	}
	var document struct {
		Tenancy     string `yaml:"tenancy_ocid_envvar"`
		User        string `yaml:"user_ocid_envvar"`
		Fingerprint string `yaml:"oci_fingerprint_envvar"`
		KeyPath     string `yaml:"oci_private_key_path_envvar"`
	}
	if err := node.Decode(&document); err != nil {
		return err
	}
	if decoded.OciTenancyOCIDEnvVar == "" {
		decoded.OciTenancyOCIDEnvVar = document.Tenancy
	}
	if decoded.OciUserOCIDEnvVar == "" {
		decoded.OciUserOCIDEnvVar = document.User
	}
	if decoded.OciFingerprintEnvVar == "" {
		decoded.OciFingerprintEnvVar = document.Fingerprint
	}
	if decoded.OciPrivateKeyFilePathEnvVar == "" {
		decoded.OciPrivateKeyFilePathEnvVar = document.KeyPath
	}
	*a = ProviderAuth(decoded)
	return nil
}
