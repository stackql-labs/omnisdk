package starlark_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/docparse/dsl"
	language "github.com/stackql-labs/omnisdk/pkg/docparse/dsl/starlark"
)

func TestExpressionsProgramsAndRegexp(t *testing.T) {
	evaluator := language.New()
	for _, test := range []struct{ program, want string }{
		{`{"id": inbox["id"], "big": inbox["big"]}`, `{"big":9007199254740993,"id":"db-1"}`},
		{"value = inbox[\"id\"]\nresult = {\"id\": value}", `{"id":"db-1"}`},
		{`re.fullmatch("db-[0-9]+", inbox["id"])`, `true`},
		{`re.search("[0-9]+", inbox["id"])`, `true`},
		{`edges["detail"].present and edges["detail"].value["state"] == "RUNNING"`, `true`},
	} {
		t.Run(test.program, func(t *testing.T) {
			output, err := evaluator.Eval(dsl.Context{Edges: map[string]map[string]any{"detail": {"present": true, "value": map[string]any{"state": "RUNNING"}, "done": false, "error": nil}}}, test.program, []byte(`{"id":"db-1","big":9007199254740993}`))
			if err != nil || string(output) != test.want {
				t.Fatalf("output=%s err=%v", output, err)
			}
		})
	}
}
func TestAOTRejectsUnboundedAndDynamicPrograms(t *testing.T) {
	for _, program := range []string{
		`"x" * 1000000000000`,
		`[x for x in inbox]`,
		"for x in inbox:\n    result = x",
		"def f():\n    return f()\nresult = f()",
		`inbox.update({"x": True})`,
		`re.search(inbox["pattern"], inbox["text"])`,
		`re.search("[", "text")`,
		`unknown(inbox)`,
		`list(range(1000000000))`,
		"len = list\nany = range\nresult = len(any(1000000000000))",
		strings.Repeat(" ", language.MaxSourceBytes+1),
		"x = inbox\n" + strings.Repeat("x = (x, x)\n", 20) + "result = x == x",
	} {
		if err := language.Validate(program); err == nil {
			t.Errorf("accepted unsafe program %q", program)
		}
	}
}
func TestCancellationAndFrozenInput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := language.New().Eval(dsl.Context{Execution: ctx}, `True`, []byte(`{}`)); err == nil {
		t.Fatal("ignored cancellation")
	}
	if _, err := language.New().Eval(dsl.Context{}, `inbox["x"] = True`, []byte(`{}`)); err == nil {
		t.Fatal("allowed input mutation")
	}
}
