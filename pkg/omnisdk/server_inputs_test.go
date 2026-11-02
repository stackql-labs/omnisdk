package omnisdk_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
)

type inputMethod struct {
	omnisdk.MethodSignature
	specs     map[string]omnisdk.ServerVariableSpec
	transport map[string]string
}

func (m inputMethod) Params() []omnisdk.ParamSignature {
	return []omnisdk.ParamSignature{inputParam{name: "host"}}
}
func (m inputMethod) ServerVariableSpecs() map[string]omnisdk.ServerVariableSpec { return m.specs }
func (m inputMethod) ServerTransportDefaults() map[string]string                 { return m.transport }

type inputParam struct {
	omnisdk.ParamSignature
	name string
}

func (p inputParam) Name() string { return p.name }

func TestServerInputResolutionSharedPrecedence(t *testing.T) {
	spec := omnisdk.ServerVariableSpec{Default: "ignored", Environment: "DIRECT", Selector: &omnisdk.EnvVarSelector{EnvVar: "CLOUD", Default: "aws", Values: map[string]string{"aws": "aws.test", "azure": "azure.test"}}}
	method := inputMethod{specs: map[string]omnisdk.ServerVariableSpec{"host": spec}, transport: map[string]string{"scheme": "https"}}
	doc := omnisdk.DocMethod{RequiredParams: []string{"host", "scheme", "region"}, ServerVariables: method.specs, TransportDefaults: method.transport}
	cases := []struct {
		name          string
		env, defaults map[string]string
		explicit      bool
		want          string
		failure       bool
	}{
		{name: "selector default", want: "aws.test"},
		{name: "selector choice", env: map[string]string{"CLOUD": " azure "}, want: "azure.test"},
		{name: "direct override", env: map[string]string{"DIRECT": " direct.test/// ", "CLOUD": "invalid"}, want: "direct.test"},
		{name: "session override", env: map[string]string{"CLOUD": "invalid"}, defaults: map[string]string{"host": "session.test"}, want: "session.test"},
		{name: "explicit override", env: map[string]string{"CLOUD": "invalid"}, defaults: map[string]string{"host": "session.test"}, explicit: true},
		{name: "invalid choice", env: map[string]string{"CLOUD": "invalid"}, failure: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lookup := func(name string) (string, bool) { v, ok := tc.env[name]; return v, ok }
			supplied := func(name string) bool { return name == "host" && tc.explicit }
			for _, resolver := range []omnisdk.ServerInputResolver{
				omnisdk.NewMethodInputResolver([]omnisdk.MethodSignature{method}, tc.defaults, lookup),
				omnisdk.NewDocMethodInputResolver(doc, tc.defaults, lookup),
			} {
				bindings, err := resolver.Resolve(supplied)
				required, requiredErr := resolver.Required(doc.RequiredParams, supplied)
				if tc.failure {
					if err == nil || requiredErr == nil {
						t.Fatal("execution and metadata must both reject invalid selector")
					}
					continue
				}
				if err != nil || requiredErr != nil {
					t.Fatalf("resolve=%v required=%v", err, requiredErr)
				}
				if bindings["host"] != tc.want || bindings["scheme"] != "https" {
					t.Fatalf("bindings=%v", bindings)
				}
				if !reflect.DeepEqual(required, []string{"region"}) {
					t.Fatalf("required=%v", required)
				}
			}
		})
	}
}

func TestServerInputConflictsAndMissingSources(t *testing.T) {
	first := inputMethod{specs: map[string]omnisdk.ServerVariableSpec{"host": {Environment: "FIRST"}}, transport: map[string]string{"scheme": "https"}}
	second := inputMethod{specs: map[string]omnisdk.ServerVariableSpec{"host": {Environment: "SECOND"}}, transport: map[string]string{"scheme": "http"}}
	lookup := func(name string) (string, bool) { return name + ".test", true }
	resolver := omnisdk.NewMethodInputResolver([]omnisdk.MethodSignature{first, second}, nil, lookup)
	if _, err := resolver.Resolve(nil); err == nil || !strings.Contains(err.Error(), "conflicting server bindings") {
		t.Fatalf("conflict=%v", err)
	}
	supplied := func(name string) bool { return name == "host" }
	if _, err := resolver.Resolve(supplied); err == nil || !strings.Contains(err.Error(), "conflicting transport defaults") {
		t.Fatalf("transport conflict=%v", err)
	}
	resolver = omnisdk.NewMethodInputResolver([]omnisdk.MethodSignature{first}, nil, func(string) (string, bool) { return "", false })
	if _, err := resolver.Resolve(nil); err == nil {
		t.Fatal("missing source accepted")
	}
	required, err := resolver.Required([]string{"host", "scheme"}, nil)
	if err != nil || !reflect.DeepEqual(required, []string{"host"}) {
		t.Fatalf("required=%v err=%v", required, err)
	}
}
