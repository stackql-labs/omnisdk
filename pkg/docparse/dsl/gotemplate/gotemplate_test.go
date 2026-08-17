package gotemplate_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/docparse/dsl"
	"github.com/stackql-labs/omnisdk/pkg/docparse/dsl/gotemplate"
	"github.com/stackql-labs/omnisdk/pkg/docparse/stackqldoc"
)

func registry(t *testing.T) dsl.Registry {
	t.Helper()
	r, err := dsl.NewRegistry(gotemplate.Evaluators()...)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// The one that matters: take the program the DOCUMENT ships for DescribeInstances, run it over
// EC2-shaped XML, and get the document's own JSON shape out. Nothing here is a fixture program —
// it is read straight from ec2.yaml.
func TestDocumentsOwnResponseProgram(t *testing.T) {
	b, err := os.ReadFile("../../stackqldoc/testdata/aws/v00.00.00000/services/ec2.yaml")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := stackqldoc.Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	ex, err := doc.Select("instances")
	if err != nil {
		t.Fatal(err)
	}
	tr := ex.Response().Transform()

	const xml = `<?xml version="1.0" encoding="UTF-8"?>
<DescribeInstancesResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/">
  <nextToken>tok-123</nextToken>
  <reservationSet>
    <item>
      <reservationId>r-1</reservationId>
      <instancesSet>
        <item><instanceId>i-aaa</instanceId><instanceType>t3.micro</instanceType></item>
        <item><instanceId>i-bbb</instanceId><instanceType>t3.small</instanceType></item>
      </instancesSet>
    </item>
  </reservationSet>
</DescribeInstancesResponse>`

	out, err := registry(t).Eval(tr.Type(), tr.Body(), []byte(xml))
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("program did not emit valid JSON: %v\n%s", err, out)
	}
	items, _ := got["line_items"].([]any)
	if len(items) != 2 {
		t.Fatalf("line_items = %v, want the two instances flattened out of reservationSet", got["line_items"])
	}
	// the program lifts the items out of their nested envelope; this document passes each item's own
	// shape through rather than renaming fields
	first, _ := items[0].(map[string]any)
	if first["instanceId"] != "i-aaa" || first["instanceType"] != "t3.micro" {
		t.Fatalf("first item = %v", first)
	}
}

// A single XML element decodes to a scalar, not a one-element slice — the ambiguity the documents
// branch on with kindOf. The same program must handle it.
func TestSingleItemIsNotASlice(t *testing.T) {
	b, _ := os.ReadFile("../../stackqldoc/testdata/aws/v00.00.00000/services/ec2.yaml")
	doc, err := stackqldoc.Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	ex, _ := doc.Select("instances")
	tr := ex.Response().Transform()

	const xml = `<DescribeInstancesResponse>
  <reservationSet><item><instancesSet>
    <item><instanceId>i-only</instanceId></item>
  </instancesSet></item></reservationSet>
</DescribeInstancesResponse>`

	out, err := registry(t).Eval(tr.Type(), tr.Body(), []byte(xml))
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	items, _ := got["line_items"].([]any)
	if len(items) != 1 {
		t.Fatalf("line_items = %v, want one", got["line_items"])
	}
	if first, _ := items[0].(map[string]any); first["instanceId"] != "i-only" {
		t.Fatalf("item = %v", items[0])
	}
}

// The request language binds "." to a STRING, and turns a JSON request context into a form query.
func TestRequestProgramBuildsFormQuery(t *testing.T) {
	prog := `{{- $body := jsonMapFromString . -}}
{{- $query := "Action=DescribeInstances" -}}
{{- range $k, $v := $body -}}
  {{- if eq (kindOf $v) "slice" -}}
    {{- range $i, $sub := $v -}}
      {{- $query = printf "%s&%s.%d=%s" $query $k (plus1 $i) (urlquery $sub) -}}
    {{- end -}}
  {{- else -}}
    {{- $query = printf "%s&%s=%s" $query $k (urlquery $v) -}}
  {{- end -}}
{{- end -}}
{{- $query -}}`

	out, err := registry(t).Eval(gotemplate.TypeText, prog, []byte(`{"InstanceId":["i-a","i-b"],"MaxResults":"5"}`))
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	for _, want := range []string{"Action=DescribeInstances", "InstanceId.1=i-a", "InstanceId.2=i-b", "MaxResults=5"} {
		if !strings.Contains(got, want) {
			t.Fatalf("query %q missing %q", got, want)
		}
	}
}

// An unknown language must fail loudly and say what IS supported — a document naming an evaluator
// nothing implements is a gap to fix, never something to skip past.
func TestUnknownLanguageFailsLoudly(t *testing.T) {
	_, err := registry(t).Eval("jsonnet_v1", "{}", nil)
	if err == nil || !strings.Contains(err.Error(), "no evaluator") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), gotemplate.TypeMXJ) {
		t.Fatalf("error should list supported types, got %v", err)
	}
}

func TestDuplicateEvaluatorRejected(t *testing.T) {
	if _, err := dsl.NewRegistry(gotemplate.MXJ(), gotemplate.MXJ()); err == nil {
		t.Fatal("two implementations of one language must be rejected")
	}
}
