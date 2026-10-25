package omnisdk

import "github.com/stackql/stackql-provider-registry/signing/pqsig"

// RegistryConfig locates provider documents and configures signature verification.
type RegistryConfig struct {
	RegistryURL      string                `json:"url" yaml:"url"`
	SrcPrefix        *string               `json:"srcPrefix" yaml:"srcPrefix"`
	DistPrefix       *string               `json:"distPrefix" yaml:"distPrefix"`
	AllowSrcDownload bool                  `json:"allowSrcDownload" yaml:"allowSrcDownload"`
	LocalDocRoot     string                `json:"localDocRoot" yaml:"localDocRoot"`
	VerifyConfig     *pqsig.VerifierConfig `json:"verifyConfig" yaml:"verifyConfig"`
}
