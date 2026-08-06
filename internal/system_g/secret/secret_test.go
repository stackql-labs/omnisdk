package secret

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRequirePrecedenceDirectOverEnv(t *testing.T) {
	t.Setenv("OMNI_TEST_K", "from-env")
	got, err := Require("k", Literal("direct"), Env("OMNI_TEST_K")).Resolve()
	if err != nil || got != "direct" {
		t.Fatalf("got %q, %v; want direct (literal wins over env)", got, err)
	}
}

func TestRequireFallsBackToEnv(t *testing.T) {
	t.Setenv("OMNI_TEST_K", "from-env")
	got, err := Require("k", Literal(""), Env("OMNI_TEST_K")).Resolve()
	if err != nil || got != "from-env" {
		t.Fatalf("got %q, %v; want from-env", got, err)
	}
}

func TestRequireErrorsWhenNoneProvided(t *testing.T) {
	if _, err := Require("the key", Literal(""), Env("OMNI_UNSET_XYZ")).Resolve(); err == nil {
		t.Error("want an error naming the missing key")
	}
}

func TestFileSource(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sa.json")
	if err := os.WriteFile(p, []byte(`{"k":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Require("sa", File(""), File(p)).Resolve()
	if err != nil || got != `{"k":1}` {
		t.Fatalf("got %q, %v; want file contents", got, err)
	}
}

func TestFileMissingErrors(t *testing.T) {
	if _, err := File(filepath.Join(t.TempDir(), "nope")).Resolve(); err == nil {
		t.Error("a set-but-unreadable file must error")
	}
}

func TestOptionalDefaultsToTrailingLiteral(t *testing.T) {
	if got := Optional(Literal(""), Env("OMNI_UNSET_XYZ"), Literal("fallback")); got != "fallback" {
		t.Errorf("got %q, want fallback", got)
	}
	if got := Optional(Literal(""), Env("OMNI_UNSET_XYZ")); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}
