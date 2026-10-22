package auth

import (
	"bufio"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/httpx"
)

func ociMethod(cfg AuthStruct) (Method, error) {
	value := func(literal, variable string) string {
		if variable != "" {
			return os.Getenv(variable)
		}
		return literal
	}
	tenancy, user, fp := value(cfg.TenancyOCID, cfg.TenancyOCIDEnvVar), value(cfg.UserOCID, cfg.UserOCIDEnvVar), value(cfg.Fingerprint, cfg.FingerprintEnvVar)
	key, pass := value(cfg.PrivateKey, cfg.PrivateKeyEnvVar), value(cfg.Passphrase, cfg.PassphraseEnvVar)
	keyPath := value(cfg.PrivateKeyPath, cfg.PrivateKeyPathEnvVar)
	if key == "" && keyPath != "" {
		b, err := os.ReadFile(expandOCIHome(keyPath))
		if err != nil {
			return nil, fmt.Errorf("cannot compose OCI signing credentials: read private key: %w", err)
		}
		key = string(b)
	}
	rawSpecified := false
	for _, field := range []string{cfg.TenancyOCID, cfg.TenancyOCIDEnvVar, cfg.UserOCID, cfg.UserOCIDEnvVar, cfg.Fingerprint, cfg.FingerprintEnvVar, cfg.PrivateKey, cfg.PrivateKeyEnvVar, cfg.PrivateKeyPath, cfg.PrivateKeyPathEnvVar} {
		if field != "" {
			rawSpecified = true
			break
		}
	}
	if !rawSpecified {
		file := cfg.ConfigFilePath
		if file == "" {
			file = "~/.oci/config"
		}
		profile := cfg.Profile
		if profile == "" {
			profile = "DEFAULT"
		}
		values, err := ociConfig(expandOCIHome(file), profile)
		if err != nil {
			return nil, fmt.Errorf("cannot compose OCI signing credentials: %w", err)
		}
		tenancy, user, fp = values["tenancy"], values["user"], values["fingerprint"]
		keyPath = values["key_file"]
		if keyPath != "" {
			keyPath = expandOCIHome(keyPath)
			if !filepath.IsAbs(keyPath) {
				keyPath = filepath.Join(filepath.Dir(expandOCIHome(file)), keyPath)
			}
			b, err := os.ReadFile(keyPath)
			if err != nil {
				return nil, fmt.Errorf("cannot compose OCI signing credentials: read private key: %w", err)
			}
			key = string(b)
		}
		if pass == "" {
			pass = values["pass_phrase"]
		}
	}
	if tenancy == "" || user == "" || fp == "" || key == "" {
		return nil, fmt.Errorf("cannot compose OCI signing credentials: tenancy_ocid, user_ocid, fingerprint and a private key are all required when any is supplied")
	}
	block, _ := pem.Decode([]byte(key))
	if block == nil {
		return nil, fmt.Errorf("OCI signing private key is not PEM")
	}
	raw := block.Bytes
	if x509.IsEncryptedPEMBlock(block) {
		var err error
		raw, err = x509.DecryptPEMBlock(block, []byte(pass))
		if err != nil {
			return nil, fmt.Errorf("OCI private key cannot be decrypted: %w", err)
		}
	}
	var rsaKey *rsa.PrivateKey
	var err error
	if block.Type == "RSA PRIVATE KEY" {
		rsaKey, err = x509.ParsePKCS1PrivateKey(raw)
	} else {
		var parsed any
		parsed, err = x509.ParsePKCS8PrivateKey(raw)
		if err == nil {
			var ok bool
			rsaKey, ok = parsed.(*rsa.PrivateKey)
			if !ok {
				err = fmt.Errorf("private key is not RSA")
			}
		}
	}
	if err != nil {
		return nil, fmt.Errorf("OCI private key cannot be parsed: %w", err)
	}
	return FromTransform(KindOCI, ociSigner{key: rsaKey, keyID: tenancy + "/" + user + "/" + fp}), nil
}

func expandOCIHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

func ociConfig(path, profile string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	values := map[string]string{}
	section := ""
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}
		if section != profile {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			values[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(values) == 0 {
		return nil, fmt.Errorf("OCI config profile %q not found", profile)
	}
	return values, nil
}

type ociSigner struct {
	key   *rsa.PrivateKey
	keyID string
}

func (s ociSigner) Apply(in facade.Page) (facade.Record, error) {
	u, err := url.Parse(httpx.URL(in))
	if err != nil {
		return nil, err
	}
	if u.RawQuery == "" {
		u.ForceQuery = false
	}
	header := httpx.Header(in).Clone()
	if header == nil {
		header = make(http.Header)
	}
	body := httpx.ReqBody(in)
	method := httpx.Method(in)
	header.Set("Date", time.Now().UTC().Format(http.TimeFormat))
	// A caller-supplied x-date would override Date in OCI's verifier.
	header.Del("X-Date")
	names := []string{"date", "(request-target)", "host"}
	lines := []string{"date: " + header.Get("Date"), "(request-target): " + strings.ToLower(method) + " " + u.RequestURI(), "host: " + u.Host}
	if method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch {
		digest := sha256.Sum256(body)
		header.Set("Content-Length", strconv.Itoa(len(body)))
		if header.Get("Content-Type") == "" {
			header.Set("Content-Type", "application/json")
		}
		header.Set("X-Content-Sha256", base64.StdEncoding.EncodeToString(digest[:]))
		for _, name := range []string{"content-length", "content-type", "x-content-sha256"} {
			names = append(names, name)
			lines = append(lines, name+": "+header.Get(name))
		}
	}
	digest := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	signature, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, digest[:])
	if err != nil {
		return nil, err
	}
	header.Set("Authorization", fmt.Sprintf("Signature version=\"1\",keyId=%q,algorithm=\"rsa-sha256\",headers=%q,signature=%q", s.keyID, strings.Join(names, " "), base64.StdEncoding.EncodeToString(signature)))
	return httpx.NewRequestRecord(method, u.String(), header, body), nil
}
