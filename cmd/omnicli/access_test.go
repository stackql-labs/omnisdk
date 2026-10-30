package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestAccessCLI(t *testing.T) {
	if os.Getenv("OMNI_ACCESS_TEST_CHILD") == "1" {
		for i, a := range os.Args {
			if a == "--" {
				os.Args = append([]string{"omnicli"}, os.Args[i+1:]...)
				main()
				return
			}
		}
		os.Exit(2)
	}
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"items":[{"name":"allowed"}]}`)
	}))
	defer srv.Close()
	dir := t.TempDir()
	doc := filepath.Join(dir, "doc.yaml")
	text := `openapi: 3.0.0
servers: [{url: 'http://example.test'}]
paths:
  /items:
    get:
      operationId: listItems
      responses:
        '200':
          content:
            application/json:
              schema:
                type: object
                properties:
                  items: {type: array, items: {type: object, properties: {name: {type: string}}}}
components:
  x-stackQL-resources:
    items:
      methods:
        list:
          operation: {$ref: '#/paths/~1items/get'}
          response: {mediaType: application/json, objectKey: items}
      sqlVerbs:
        select: [{$ref: '#/components/x-stackQL-resources/items/methods/list'}]
`
	if err := os.WriteFile(doc, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("caller-secret"))
	for _, tc := range []struct {
		name, token                  string
		operation, credential, valid bool
	}{
		{"allowed", "caller-secret", true, true, true},
		{"bad credential", "wrong", true, true, false},
		{"operation denied", "caller-secret", false, true, false},
		{"provider credential denied", "caller-secret", true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rules := []map[string]string{{"subject": "group:readers", "resource": "command/omnicli/doc-select", "action": "execute"}}
			if tc.operation {
				rules = append(rules, map[string]string{"subject": "group:readers", "resource": "operation/demo/**", "action": "execute"})
			}
			if tc.credential {
				rules = append(rules, map[string]string{"subject": "group:readers", "resource": "credential/demo", "action": "use"})
			}
			cfg := map[string]any{"issuer": "test", "mechanism": "bearer", "tokens": []map[string]string{{"subject": "alice", "sha256": hex.EncodeToString(digest[:])}}, "directory": map[string]any{"groups": []string{"readers"}, "principals": []map[string]any{{"id": "alice", "identities": []map[string]string{{"issuer": "test", "subject": "alice"}}, "groups": []string{"readers"}}}}, "rules": rules}
			raw, _ := json.Marshal(cfg)
			config := filepath.Join(dir, "access.json")
			if err := os.WriteFile(config, raw, 0600); err != nil {
				t.Fatal(err)
			}
			before := calls.Load()
			cmd := exec.Command(os.Args[0], "-test.run=^TestAccessCLI$", "--", "doc-select", "demo", doc, "items", "--endpoint", srv.URL, "--access-config", config, "--caller-token-env", "TEST_CALLER_TOKEN")
			cmd.Env = append(os.Environ(), "OMNI_ACCESS_TEST_CHILD=1", "TEST_CALLER_TOKEN="+tc.token)
			out, err := cmd.CombinedOutput()
			if (err == nil) != tc.valid {
				t.Fatalf("err=%v output=%s", err, out)
			}
			if tc.valid && !strings.Contains(string(out), "allowed") {
				t.Fatalf("missing rows: %s", out)
			}
			if !tc.valid && calls.Load() != before {
				t.Fatal("denied caller reached provider")
			}
		})
	}
}

func TestCallerInfoCLI(t *testing.T) {
	digest := sha256.Sum256([]byte("caller-info-secret"))
	for _, tc := range []struct {
		name, command, token, want string
		groups                     []string
		configured, permitted      bool
	}{
		{"identity", "whoami", "caller-info-secret", `{"principal":"alice"}`, []string{"readers", "ops"}, true, true},
		{"memberships", "groups", "caller-info-secret", `{"groups":["readers","ops"]}`, []string{"readers", "ops"}, true, true},
		{"no memberships", "groups", "caller-info-secret", `{"groups":[]}`, nil, true, true},
		{"no session", "whoami", "", "no authenticated caller", nil, false, false},
		{"no session groups", "groups", "", "no authenticated caller", nil, false, false},
		{"bad token", "whoami", "wrong", "invalid bearer credential", nil, true, true},
		{"denied identity", "whoami", "caller-info-secret", "access denied", nil, true, false},
		{"denied groups", "groups", "caller-info-secret", "access denied", nil, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"-test.run=^TestAccessCLI$", "--", tc.command}
			if tc.configured {
				rules := []map[string]string{}
				if tc.permitted {
					rules = append(rules, map[string]string{"subject": "user:alice", "resource": "command/omnicli/" + tc.command, "action": "execute"})
				}
				cfg := map[string]any{"issuer": "test", "mechanism": "bearer", "tokens": []map[string]string{{"subject": "alice", "sha256": hex.EncodeToString(digest[:])}}, "directory": map[string]any{"groups": []string{"readers", "ops"}, "principals": []map[string]any{{"id": "alice", "identities": []map[string]string{{"issuer": "test", "subject": "alice"}}, "groups": tc.groups}}}, "rules": rules}
				raw, err := json.Marshal(cfg)
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(t.TempDir(), "access.json")
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
				args = append(args, "--access-config", path, "--caller-token-env", "TEST_CALLER_TOKEN")
			}
			cmd := exec.Command(os.Args[0], args...)
			cmd.Env = append(os.Environ(), "OMNI_ACCESS_TEST_CHILD=1", "TEST_CALLER_TOKEN="+tc.token)
			out, err := cmd.CombinedOutput()
			valid := tc.configured && tc.permitted && tc.token == "caller-info-secret"
			if (err == nil) != valid {
				t.Fatalf("err=%v output=%s", err, out)
			}
			if valid {
				first, _, _ := strings.Cut(string(out), "\n")
				if first != tc.want {
					t.Fatalf("output=%q want=%q", first, tc.want)
				}
			} else if !strings.Contains(string(out), tc.want) {
				t.Fatalf("output=%s want error %q", out, tc.want)
			}
			if strings.Contains(string(out), "caller-info-secret") {
				t.Fatal("caller credential leaked")
			}
		})
	}
}
