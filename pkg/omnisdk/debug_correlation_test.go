package omnisdk_test

import (
	"strings"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
)

func TestDebugCorrelationConfig(t *testing.T) {
	for _, test := range []struct {
		name, field string
		wantError   bool
	}{
		{"absent", "", false},
		{"empty-object", `,"debug_correlation_param":{}`, true},
		{"empty-target", `,"debug_correlation_param":{"target":""}`, true},
		{"standard", `,"debug_correlation_param":{"target":"label","transformer":"standard"}`, true},
		{"blank-transformer", `,"debug_correlation_param":{"target":"label","transformer":""}`, true},
		{"unknown-transformer", `,"debug_correlation_param":{"target":"label","transformer":"guess"}`, true},
		{"unknown-option", `,"debug_correlation_param":{"target":"label","magic":true}`, true},
		{"wrong-shape", `,"debug_correlation_param":"label"`, true},
		{"old-key", `,"correlation_param":"label"`, true},
		{"old-camel-key", `,"correlationParam":"label"`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			resources, err := omnisdk.ParseResources([]byte(`[{"key":"one","provider":"widgets","address":"things.widgets","desired":{}` + test.field + `}]`))
			if (err != nil) != test.wantError {
				t.Fatalf("error=%v", err)
			}
			if err == nil && strings.Contains(test.field, `"target":"label"`) && resources[0].DebugCorrelationParam().Target != "label" {
				t.Fatal("target lost")
			}
		})
	}
}
