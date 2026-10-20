// Package arch holds architecture tests: invariants about dependency direction that the compiler
// cannot state. Go already rejects import cycles, so what needs guarding is drift — a contract
// package quietly acquiring an implementation dependency, which is how a layered design rots
// without anything failing to build.
package arch_test

import (
	"os/exec"
	"strings"
	"testing"
)

const module = "github.com/stackql-labs/omnisdk"

// deps returns every package in this module that pkg depends on, transitively, excluding itself.
func deps(t *testing.T, pkg string) []string {
	t.Helper()
	cmd := exec.Command("go", "list", "-deps", pkg)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps %s: %v: %s", pkg, err, stderr.String())
	}
	var got []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.HasPrefix(line, module) && line != pkg {
			got = append(got, line)
		}
	}
	return got
}

// facade is the contract root that most of the tree imports. It must stay standard-library only:
// the moment it depends on an implementation, every importer inherits that dependency and the
// direction of the design is no longer enforceable.
func TestFacadeDependsOnStdlibOnly(t *testing.T) {
	if got := deps(t, module+"/internal/system_g/facade"); len(got) > 0 {
		t.Errorf("facade depends on %v, want stdlib only", got)
	}
}

// query is what a front end builds. It must stay standard-library only, so a front end in another
// language's toolchain depends on it and nothing else of the module.
func TestQueryDependsOnStdlibOnly(t *testing.T) {
	for _, pkg := range []string{"/pkg/query", "/pkg/cache", "/pkg/encode"} {
		if got := deps(t, module+pkg); len(got) > 0 {
			t.Errorf("%s depends on %v, want stdlib only", pkg, got)
		}
	}
	// sqlfn renders a value as every sink does, through encode — itself standard-library only.
	for _, d := range deps(t, module+"/pkg/sqlfn") {
		if d != module+"/pkg/encode" {
			t.Errorf("pkg/sqlfn depends on %s, want pkg/encode and stdlib only", d)
		}
	}
}

// stream is what a host writes rows through. It sees the encoder and the standard library only,
// so a host takes the sinks without taking the engine.
func TestStreamDependsOnEncodeOnly(t *testing.T) {
	for _, d := range deps(t, module+"/pkg/stream") {
		if d != module+"/pkg/encode" {
			t.Errorf("pkg/stream depends on %s, want pkg/encode and stdlib only", d)
		}
	}
}

// These are disjoint from System-G: the executor calls them, never the reverse. They
// may see the contracts and nothing else.
func TestLeafImplementationsSeeContractsOnly(t *testing.T) {
	for _, pkg := range []string{
		module + "/internal/ledger",
		module + "/internal/merge",
		module + "/internal/journal",
		module + "/internal/lease",
		module + "/internal/semantics",
		module + "/internal/analyze",
	} {
		for _, d := range deps(t, pkg) {
			if d != module+"/internal/system_g/facade" {
				t.Errorf("%s depends on %s, want facade only", pkg, d)
			}
		}
	}
}
