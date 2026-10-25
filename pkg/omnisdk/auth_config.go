package omnisdk

import "net/url"

// AuthConfig carries the legacy StackQL credential configuration. Provider
// execution uses Auth; hosts translate this input without carrying database
// configuration into the SDK.
type AuthConfig struct {
	Scopes                  []string   `json:"scopes,omitempty" yaml:"scopes,omitempty"`
	Type                    string     `json:"type" yaml:"type"`
	ValuePrefix             string     `json:"valuePrefix" yaml:"valuePrefix"`
	ID                      string     `json:"-" yaml:"-"`
	KeyID                   string     `json:"keyID" yaml:"keyID"`
	KeyIDEnvVar             string     `json:"keyIDenvvar" yaml:"keyIDenvvar"`
	KeyFilePath             string     `json:"credentialsfilepath" yaml:"credentialsfilepath"`
	KeyFilePathEnvVar       string     `json:"credentialsfilepathenvvar" yaml:"credentialsfilepathenvvar"`
	KeyEnvVar               string     `json:"credentialsenvvar" yaml:"credentialsenvvar"`
	APIKeyStr               string     `json:"api_key" yaml:"api_key"`
	APISecretStr            string     `json:"api_secret" yaml:"api_secret"`
	Username                string     `json:"username" yaml:"username"`
	Password                string     `json:"password" yaml:"password"`
	EnvVarAPIKeyStr         string     `json:"api_key_var" yaml:"api_key_var"`
	EnvVarAPISecretStr      string     `json:"api_secret_var" yaml:"api_secret_var"`
	EnvVarUsername          string     `json:"username_var" yaml:"username_var"`
	EnvVarPassword          string     `json:"password_var" yaml:"password_var"`
	EncodedBasicCredentials string     `json:"-" yaml:"-"`
	Subject                 string     `json:"sub" yaml:"sub"`
	Active                  bool       `json:"-" yaml:"-"`
	Location                string     `json:"location" yaml:"location"`
	Name                    string     `json:"name" yaml:"name"`
	TokenURL                string     `json:"token_url" yaml:"token_url"`
	GrantType               string     `json:"grant_type" yaml:"grant_type"`
	ClientID                string     `json:"client_id" yaml:"client_id"`
	ClientSecret            string     `json:"client_secret" yaml:"client_secret"`
	ClientIDEnvVar          string     `json:"client_id_env_var" yaml:"client_id_env_var"`
	ClientSecretEnvVar      string     `json:"client_secret_env_var" yaml:"client_secret_env_var"`
	Values                  url.Values `json:"values" yaml:"values"`
	AuthStyle               int        `json:"auth_style" yaml:"auth_style"`
	AccountID               string     `json:"account_id" yaml:"account_id"`
	AccoountIDEnvVar        string     `json:"account_id_env_var" yaml:"account_id_var"`
	AwsRoleArn              string     `json:"aws_role_arn" yaml:"aws_role_arn"`
	AwsRoleArnEnvVar        string     `json:"aws_role_arn_env_var" yaml:"aws_role_arn_env_var"`
	AwsRoleSessionName      string     `json:"aws_role_session_name" yaml:"aws_role_session_name"`
	AwsRoleExternalID       string     `json:"aws_role_external_id" yaml:"aws_role_external_id"`
	AwsRoleExternalIDEnvVar string     `json:"aws_role_external_id_env_var" yaml:"aws_role_external_id_env_var"`
	AwsStsRegion            string     `json:"aws_sts_region" yaml:"aws_sts_region"`
	AwsStsEndpoint          string     `json:"aws_sts_endpoint" yaml:"aws_sts_endpoint"`
	AwsRoleDurationSeconds  int32      `json:"aws_role_duration_seconds" yaml:"aws_role_duration_seconds"`
	// OCI request signing (oci_signing_v1). Raw values (twelve-factor style)
	// take precedence; otherwise the OCI config file convention (~/.oci/config)
	// shared by every OCI tool applies.
	OciConfigFilePath           string `json:"config_file_path" yaml:"config_file_path"`
	OciProfile                  string `json:"profile" yaml:"profile"`
	OciTenancyOCID              string `json:"tenancy_ocid" yaml:"tenancy_ocid"`
	OciTenancyOCIDEnvVar        string `json:"tenancy_ocid_env_var" yaml:"tenancy_ocid_env_var"`
	OciUserOCID                 string `json:"user_ocid" yaml:"user_ocid"`
	OciUserOCIDEnvVar           string `json:"user_ocid_env_var" yaml:"user_ocid_env_var"`
	OciFingerprint              string `json:"fingerprint" yaml:"fingerprint"`
	OciFingerprintEnvVar        string `json:"fingerprint_env_var" yaml:"fingerprint_env_var"`
	OciPrivateKey               string `json:"private_key" yaml:"private_key"`
	OciPrivateKeyEnvVar         string `json:"private_key_env_var" yaml:"private_key_env_var"`
	OciPrivateKeyFilePath       string `json:"private_key_path" yaml:"private_key_path"`
	OciPrivateKeyFilePathEnvVar string `json:"private_key_path_env_var" yaml:"private_key_path_env_var"`
	OciPassphrase               string `json:"passphrase" yaml:"passphrase"`
	OciPassphraseEnvVar         string `json:"passphrase_env_var" yaml:"passphrase_env_var"`
	OciRegion                   string `json:"region" yaml:"region"`
	OciRegionEnvVar             string `json:"region_env_var" yaml:"region_env_var"`
}
