package auth

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Credential sources an interactive user already has: the files and tools each cloud's own CLI
// maintains. Each is where credentials are kept — never which account or region a query acts on.

// AWSKeys is an AWS access key, with a session token where it is temporary.
type AWSKeys struct{ AccessKeyID, SecretAccessKey, SessionToken string }

// AWSProfile reads profile's keys from the shared credentials file, else from the config file — its
// static keys or its credential_process. The files are AWS_SHARED_CREDENTIALS_FILE and
// AWS_CONFIG_FILE where set, else ~/.aws/credentials and ~/.aws/config.
func AWSProfile(profile string) (AWSKeys, error) {
	if profile == "" {
		profile = "default"
	}
	home, _ := os.UserHomeDir()
	credFile := orEnv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(home, ".aws", "credentials"))
	cfgFile := orEnv("AWS_CONFIG_FILE", filepath.Join(home, ".aws", "config"))
	if sec, err := iniSection(credFile, profile); err == nil {
		if k, ok := staticKeys(sec); ok {
			return k, nil
		}
	}
	name := "profile " + profile
	if profile == "default" {
		name = "default"
	}
	sec, err := iniSection(cfgFile, name)
	if err != nil {
		return AWSKeys{}, fmt.Errorf("auth: AWS profile %q: %w", profile, err)
	}
	if k, ok := staticKeys(sec); ok {
		return k, nil
	}
	if proc := sec["credential_process"]; proc != "" {
		return credentialProcess(proc)
	}
	return AWSKeys{}, fmt.Errorf("auth: AWS profile %q has no keys or credential_process (SSO and role chaining are not read)", profile)
}

func staticKeys(sec map[string]string) (AWSKeys, bool) {
	k := AWSKeys{AccessKeyID: sec["aws_access_key_id"], SecretAccessKey: sec["aws_secret_access_key"],
		SessionToken: sec["aws_session_token"]}
	return k, k.AccessKeyID != "" && k.SecretAccessKey != ""
}

// credentialProcess runs a profile's credential_process and reads the keys it prints.
func credentialProcess(command string) (AWSKeys, error) {
	ctx, cancel := context.WithTimeout(context.Background(), execTimeout)
	defer cancel()
	out, err := shell(ctx, command).Output()
	if err != nil {
		return AWSKeys{}, fmt.Errorf("auth: AWS credential_process: %w", err)
	}
	var v struct {
		Version                                    int
		AccessKeyID, SecretAccessKey, SessionToken string
	}
	if err := json.Unmarshal(out, &v); err != nil || v.AccessKeyID == "" {
		return AWSKeys{}, fmt.Errorf("auth: AWS credential_process printed no keys")
	}
	return AWSKeys{AccessKeyID: v.AccessKeyID, SecretAccessKey: v.SecretAccessKey, SessionToken: v.SessionToken}, nil
}

// iniSection reads one [section] of an AWS-style ini file.
func iniSection(path, section string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out, in := map[string]string{}, false
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";"):
		case strings.HasPrefix(line, "["):
			in = strings.TrimSpace(strings.Trim(line, "[]")) == section
		case in:
			if k, v, ok := strings.Cut(line, "="); ok {
				out[strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no section [%s] in %s", section, path)
	}
	return out, nil
}

// GoogleADCFile is where gcloud keeps application-default credentials.
func GoogleADCFile() string {
	if runtime.GOOS == "windows" {
		return filepath.Join(os.Getenv("APPDATA"), "gcloud", "application_default_credentials.json")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(orEnv("CLOUDSDK_CONFIG", filepath.Join(home, ".config", "gcloud")), "application_default_credentials.json")
}

// GoogleUser is gcloud's authorized_user credential: a refresh token and the client that minted it.
type GoogleUser struct{ ClientID, ClientSecret, RefreshToken string }

// ParseGoogleUser reads an authorized_user credential; ok is false for any other type.
func ParseGoogleUser(raw []byte) (GoogleUser, bool) {
	var v struct {
		Type         string `json:"type"`
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
		RefreshToken string `json:"refresh_token"`
	}
	if json.Unmarshal(raw, &v) != nil || v.Type != "authorized_user" {
		return GoogleUser{}, false
	}
	return GoogleUser{ClientID: v.ClientID, ClientSecret: v.ClientSecret, RefreshToken: v.RefreshToken}, true
}

// AzureCLIToken asks the Azure CLI for an access token to resource, as `az login` left it.
func AzureCLIToken(resource string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), execTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "az", "account", "get-access-token", "--resource", resource, "--output", "json").Output()
	if err != nil {
		return "", fmt.Errorf("auth: az account get-access-token: %w", err)
	}
	var v struct {
		AccessToken string `json:"accessToken"`
	}
	if err := json.Unmarshal(out, &v); err != nil || v.AccessToken == "" {
		return "", fmt.Errorf("auth: az account get-access-token printed no token")
	}
	return v.AccessToken, nil
}

// BearerMethod sends a token as a bearer header.
func BearerMethod(token string) Method {
	return static{kind: KindBearer, t: headerT{name: "Authorization", value: "Bearer " + token}}
}

func shell(ctx context.Context, command string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.CommandContext(ctx, "cmd", "/C", command)
	}
	return exec.CommandContext(ctx, "/bin/sh", "-c", command)
}

func orEnv(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
