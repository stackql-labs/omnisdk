//go:build !windows

package secretstore_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stackql-labs/omnisdk/public/secrets"
)

func TestOSBackendOptInAndNoSecretsInProcessArguments(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("no command-based OS backend")
	}
	root := t.TempDir()
	command := "secret-tool"
	if runtime.GOOS == "darwin" {
		command = "security"
	}
	payload := filepath.Join(root, "payload")
	arguments := filepath.Join(root, "arguments")
	t.Setenv("OMNI_OS_TEST_PAYLOAD", payload)
	t.Setenv("OMNI_OS_TEST_ARGUMENTS", arguments)
	script := `#!/bin/sh
printf '%s\n' "$@" >> "$OMNI_OS_TEST_ARGUMENTS"
case "$1" in
store) /bin/cat > "$OMNI_OS_TEST_PAYLOAD" ;;
-i) /bin/cat > "$OMNI_OS_TEST_PAYLOAD.input"; /usr/bin/sed -n 's/.*-w "\([^"]*\)".*/\1/p' "$OMNI_OS_TEST_PAYLOAD.input" > "$OMNI_OS_TEST_PAYLOAD" ;;
lookup|find-generic-password) /bin/cat "$OMNI_OS_TEST_PAYLOAD" ;;
clear|delete-generic-password) /bin/rm -f "$OMNI_OS_TEST_PAYLOAD" ;;
esac
`
	if err := os.WriteFile(filepath.Join(root, command), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root)
	store, err := secrets.New(secrets.Config{Backend: "os", Namespace: "test/namespace"})
	if err != nil {
		t.Fatal(err)
	}
	value := []byte("not-in-argv\n\"command-looking\"\x00")
	if err := store.Set(context.Background(), "name with quotes \" and newline\n", value); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(context.Background(), "name with quotes \" and newline\n")
	if err != nil || string(got) != string(value) {
		t.Fatalf("OS round trip failed: %v", err)
	}
	argv, err := os.ReadFile(arguments)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(argv), "not-in-argv") {
		t.Fatal("secret entered process arguments")
	}
	if err := store.Delete(context.Background(), "name with quotes \" and newline\n"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	if _, err := secrets.New(secrets.Config{Backend: "os", Namespace: "test"}); !errors.Is(err, secrets.ErrUnavailable) {
		t.Fatal("silently fell back from unavailable OS backend")
	}
	if _, err := secrets.New(secrets.Config{Path: filepath.Join(root, "default-store")}); err != nil {
		t.Fatal("file default depends on OS facility")
	}
}
