package aot_test

import (
	"strings"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
)

func TestServerVariableResolution(t *testing.T) {
	spec := aot.ServerVariableSpec{Default: "documentation-only", Environment: "DIRECT_HOST", Selector: &aot.EnvVarSelector{EnvVar: "CLOUD", Default: "aws", Values: map[string]string{"aws": "aws.example.test", "azure": "azure.example.test"}}}
	tests := []struct {
		name     string
		values   map[string]string
		want     string
		resolved bool
		failure  string
	}{
		{name: "declared default", want: "aws.example.test", resolved: true},
		{name: "selector", values: map[string]string{"CLOUD": " azure "}, want: "azure.example.test", resolved: true},
		{name: "direct override", values: map[string]string{"DIRECT_HOST": " host.example.test/// ", "CLOUD": "unknown"}, want: "host.example.test", resolved: true},
		{name: "invalid direct override", values: map[string]string{"DIRECT_HOST": "///"}, failure: "resolves to an empty value"},
		{name: "empty direct override", values: map[string]string{"DIRECT_HOST": " \t ", "CLOUD": "azure"}, want: "azure.example.test", resolved: true},
		{name: "unknown selector", values: map[string]string{"CLOUD": "Azure"}, failure: `environment variable "CLOUD" value "Azure" is not in values [aws azure]`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolver := aot.NewServerVariableResolver(spec)
			value, ok, err := resolver.Resolve("account_host", func(key string) (string, bool) { v, exists := test.values[key]; return v, exists })
			if test.failure != "" {
				if err == nil || !strings.Contains(err.Error(), test.failure) || !strings.Contains(err.Error(), "account_host") {
					t.Fatalf("error=%v", err)
				}
				return
			}
			if err != nil || value != test.want || ok != test.resolved {
				t.Fatalf("value=%q resolved=%v error=%v", value, ok, err)
			}
		})
	}
}

func TestServerVariableWithoutOptInStaysUnresolved(t *testing.T) {
	for _, spec := range []aot.ServerVariableSpec{{Default: "ignored"}, {Environment: "HOST"}, {Selector: &aot.EnvVarSelector{EnvVar: "CLOUD", Values: map[string]string{"aws": "aws.example"}}}} {
		_, ok, err := aot.NewServerVariableResolver(spec).Resolve("host", func(string) (string, bool) { return "ignored", false })
		if err != nil || ok {
			t.Fatalf("unconfigured scope resolved: %v %v", ok, err)
		}
	}
	if aot.NewServerVariableResolver(aot.ServerVariableSpec{Default: "ignored"}).Declared() {
		t.Fatal("OpenAPI default opted into resolution")
	}
}

func TestServerVariableValidationAndCopies(t *testing.T) {
	invalid := aot.ServerVariableSpec{Selector: &aot.EnvVarSelector{Default: "missing", Values: map[string]string{"aws": "aws.example"}}}
	if _, _, err := aot.NewServerVariableResolver(invalid).Resolve("host", nil); err == nil {
		t.Fatal("invalid selector default accepted")
	}
	original := aot.ServerVariableSpec{Selector: &aot.EnvVarSelector{Default: "aws", Values: map[string]string{"aws": "original.example"}}}
	resolver := aot.NewServerVariableResolver(original)
	copied := aot.CloneServerVariableSpecs(map[string]aot.ServerVariableSpec{"host": original})
	original.Selector.Values["aws"] = "changed.example"
	value, ok, err := resolver.Resolve("host", nil)
	if err != nil || !ok || value != "original.example" || copied["host"].Selector.Values["aws"] != "original.example" {
		t.Fatal("mutable metadata escaped")
	}
}
