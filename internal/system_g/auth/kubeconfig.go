package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// kubeconfig resolves a kubeconfig context into a Method: the user's credential as a request
// header (token, token file, exec plugin, basic) or a client certificate, and the cluster's CA as
// TLS. The file is Credentials inline, else CredentialsFilePath, else $KUBECONFIG, else
// ~/.kube/config — where credentials are kept, which is not scope. The context is required.
func kubeconfig(cfg AuthStruct) (Method, error) {
	if cfg.Context == "" {
		return nil, fmt.Errorf("auth: kubeconfig needs a context: which cluster's credentials to send is not a default")
	}
	raw, dir, err := kubeconfigBytes(cfg)
	if err != nil {
		return nil, err
	}
	var kc kubeConfig
	if err := yaml.Unmarshal(raw, &kc); err != nil {
		return nil, fmt.Errorf("auth: kubeconfig: %w", err)
	}
	ctx, ok := kc.context(cfg.Context)
	if !ok {
		return nil, fmt.Errorf("auth: kubeconfig has no context %q", cfg.Context)
	}
	cluster, ok := kc.cluster(ctx.Cluster)
	if !ok {
		return nil, fmt.Errorf("auth: kubeconfig context %q names cluster %q, which it does not define", cfg.Context, ctx.Cluster)
	}
	user, ok := kc.user(ctx.User)
	if !ok {
		return nil, fmt.Errorf("auth: kubeconfig context %q names user %q, which it does not define", cfg.Context, ctx.User)
	}
	t := TLS{
		CACertData: cluster.CAData, CACertFile: rel(dir, cluster.CA), InsecureSkipVerify: cluster.Insecure,
		ClientCertData: user.CertData, ClientCertFile: rel(dir, user.Cert),
		ClientKeyData: user.KeyData, ClientKeyFile: rel(dir, user.Key),
	}
	token := user.Token
	if token == "" && user.TokenFile != "" {
		b, err := os.ReadFile(rel(dir, user.TokenFile))
		if err != nil {
			return nil, fmt.Errorf("auth: kubeconfig token file: %w", err)
		}
		token = strings.TrimSpace(string(b))
	}
	if token == "" && user.Exec != nil {
		cred, err := user.Exec.run()
		if err != nil {
			return nil, err
		}
		token = cred.Token
		if cred.CertData != "" {
			t.ClientCertData = base64.StdEncoding.EncodeToString([]byte(cred.CertData))
			t.ClientKeyData = base64.StdEncoding.EncodeToString([]byte(cred.KeyData))
			t.ClientCertFile, t.ClientKeyFile = "", ""
		}
	}
	tc, err := t.config()
	if err != nil {
		return nil, fmt.Errorf("auth: kubeconfig context %q: %w", cfg.Context, err)
	}
	var m Method = static{kind: KindKubeconfig}
	switch {
	case token != "":
		m = static{kind: KindKubeconfig, t: headerT{name: "Authorization", value: "Bearer " + token}}
	case user.Username != "":
		enc := base64.StdEncoding.EncodeToString([]byte(user.Username + ":" + user.Password))
		m = static{kind: KindKubeconfig, t: headerT{name: "Authorization", value: "Basic " + enc}}
	case len(tc.Certificates) == 0:
		return nil, fmt.Errorf("auth: kubeconfig user %q has no token, certificate, exec plugin or password", ctx.User)
	}
	return tlsMethod{Method: m, tc: tc}, nil
}

func kubeconfigBytes(cfg AuthStruct) ([]byte, string, error) {
	if cfg.Credentials != "" {
		return []byte(cfg.Credentials), "", nil
	}
	path := cfg.CredentialsFilePath
	if path == "" {
		path = os.Getenv(def(cfg.CredentialsEnvVar, "KUBECONFIG"))
		if i := strings.IndexRune(path, os.PathListSeparator); i >= 0 {
			path = path[:i] // the first file of a KUBECONFIG list
		}
	}
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, "", fmt.Errorf("auth: kubeconfig: %w", err)
		}
		path = filepath.Join(home, ".kube", "config")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("auth: kubeconfig: %w", err)
	}
	return b, filepath.Dir(path), nil
}

// rel resolves a path in a kubeconfig against the file's directory, as kubectl does.
func rel(dir, p string) string {
	if p == "" || dir == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(dir, p)
}

type kubeConfig struct {
	Clusters []struct {
		Name    string `yaml:"name"`
		Cluster struct {
			CA       string `yaml:"certificate-authority"`
			CAData   string `yaml:"certificate-authority-data"`
			Insecure bool   `yaml:"insecure-skip-tls-verify"`
		} `yaml:"cluster"`
	} `yaml:"clusters"`
	Users []struct {
		Name string   `yaml:"name"`
		User kubeUser `yaml:"user"`
	} `yaml:"users"`
	Contexts []struct {
		Name    string `yaml:"name"`
		Context struct {
			Cluster string `yaml:"cluster"`
			User    string `yaml:"user"`
		} `yaml:"context"`
	} `yaml:"contexts"`
}

type kubeUser struct {
	Token     string    `yaml:"token"`
	TokenFile string    `yaml:"tokenFile"`
	Cert      string    `yaml:"client-certificate"`
	CertData  string    `yaml:"client-certificate-data"`
	Key       string    `yaml:"client-key"`
	KeyData   string    `yaml:"client-key-data"`
	Username  string    `yaml:"username"`
	Password  string    `yaml:"password"`
	Exec      *execSpec `yaml:"exec"`
}

type kubeCluster = struct {
	CA       string `yaml:"certificate-authority"`
	CAData   string `yaml:"certificate-authority-data"`
	Insecure bool   `yaml:"insecure-skip-tls-verify"`
}

func (k kubeConfig) context(name string) (struct{ Cluster, User string }, bool) {
	for _, c := range k.Contexts {
		if c.Name == name {
			return struct{ Cluster, User string }{c.Context.Cluster, c.Context.User}, true
		}
	}
	return struct{ Cluster, User string }{}, false
}

func (k kubeConfig) cluster(name string) (kubeCluster, bool) {
	for _, c := range k.Clusters {
		if c.Name == name {
			return c.Cluster, true
		}
	}
	return kubeCluster{}, false
}

func (k kubeConfig) user(name string) (kubeUser, bool) {
	for _, u := range k.Users {
		if u.Name == name {
			return u.User, true
		}
	}
	return kubeUser{}, false
}

// execSpec is a kubeconfig credential plugin: a command printing an ExecCredential.
type execSpec struct {
	Command string   `yaml:"command"`
	Args    []string `yaml:"args"`
	Env     []struct {
		Name  string `yaml:"name"`
		Value string `yaml:"value"`
	} `yaml:"env"`
	APIVersion string `yaml:"apiVersion"`
}

type execCredential struct{ Token, CertData, KeyData string }

// execTimeout bounds a credential plugin: one that hangs must not hang the query.
const execTimeout = 60 * time.Second

func (e *execSpec) run() (execCredential, error) {
	ctx, cancel := context.WithTimeout(context.Background(), execTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.Command, e.Args...)
	cmd.Env = os.Environ()
	for _, kv := range e.Env {
		cmd.Env = append(cmd.Env, kv.Name+"="+kv.Value)
	}
	// The plugin reads its request from KUBERNETES_EXEC_INFO; non-interactive, so it must not prompt.
	info, _ := json.Marshal(map[string]any{"apiVersion": e.APIVersion, "kind": "ExecCredential",
		"spec": map[string]any{"interactive": false}})
	cmd.Env = append(cmd.Env, "KUBERNETES_EXEC_INFO="+string(info))
	out, err := cmd.Output()
	if err != nil {
		return execCredential{}, fmt.Errorf("auth: kubeconfig exec %s: %w", e.Command, err)
	}
	var resp struct {
		Status struct {
			Token                 string `json:"token"`
			ClientCertificateData string `json:"clientCertificateData"`
			ClientKeyData         string `json:"clientKeyData"`
		} `json:"status"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return execCredential{}, fmt.Errorf("auth: kubeconfig exec %s printed no ExecCredential: %w", e.Command, err)
	}
	s := resp.Status
	if s.Token == "" && s.ClientCertificateData == "" {
		return execCredential{}, fmt.Errorf("auth: kubeconfig exec %s returned no token or certificate", e.Command)
	}
	return execCredential{Token: s.Token, CertData: s.ClientCertificateData, KeyData: s.ClientKeyData}, nil
}
