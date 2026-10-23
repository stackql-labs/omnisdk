package stackqldoc

import (
	"gopkg.in/yaml.v3"
	"reflect"
	"testing"
)

func TestAllOfPreservesInheritedAndLocalFieldsWithoutCycles(t *testing.T) {
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(`type: object
allOf:
  - $ref: '#/components/schemas/base'
  - properties:
      extra: {type: boolean}
properties:
  id: {type: integer}
`), &root); err != nil {
		t.Fatal(err)
	}
	var base yaml.Node
	if err := yaml.Unmarshal([]byte(`type: object
allOf:
  - $ref: '#/components/schemas/base'
properties:
  id: {type: string}
  name: {type: string}
`), &base); err != nil {
		t.Fatal(err)
	}
	schema := newSchema(root.Content[0], map[string]*yaml.Node{"base": base.Content[0]})
	if !reflect.DeepEqual(schema.Properties(), []string{"extra", "id", "name"}) {
		t.Fatalf("properties = %v", schema.Properties())
	}
	id, _ := schema.Property("id")
	if id.(*materialised).Type() != "integer" {
		t.Fatal("local property did not override inherited property")
	}
}

func TestXMLSelectionPreservesRowBeyondEnvelopeDepth(t *testing.T) {
	var root yaml.Node
	err := yaml.Unmarshal([]byte(`type: object
properties:
  DescribeInstancesResponse:
    type: object
    properties:
      reservationSet:
        type: array
        items:
          type: object
          properties:
            instancesSet:
              type: array
              items:
                allOf:
                  - type: object
                    properties:
                      instanceId: {type: string}
                      instanceType: {type: string}
`), &root)
	if err != nil {
		t.Fatal(err)
	}
	selected := selectedXMLSchema(root.Content[0], nil, "/*/reservationSet/item/instancesSet/item")
	if selected == nil || !reflect.DeepEqual(selected.Properties(), []string{"instanceId", "instanceType"}) {
		t.Fatalf("selected row = %v", selected)
	}
}

func TestXMLSelectionUsesDeclaredArrayItemName(t *testing.T) {
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(`type: object
properties:
  ResourceRecordSets:
    type: array
    items:
      allOf:
        - type: object
          properties:
            Name: {type: string}
            Type: {type: string}
        - xml: {name: ResourceRecordSet}
`), &root); err != nil {
		t.Fatal(err)
	}
	selected := selectedXMLSchema(root.Content[0], nil, "/*/ResourceRecordSets/ResourceRecordSet")
	if selected == nil || !reflect.DeepEqual(selected.Properties(), []string{"Name", "Type"}) {
		t.Fatalf("selected row = %v", selected)
	}
	if got := selectedXMLSchema(root.Content[0], nil, "/*/ResourceRecordSets/Unrelated"); got != nil {
		t.Fatal("undeclared array element unexpectedly matched")
	}
}

func TestXMLScalarSelectionDeclaresNamedColumn(t *testing.T) {
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(`type: object
properties:
  PolicyNames:
    type: array
    items: {type: string}
`), &root); err != nil {
		t.Fatal(err)
	}
	selected := selectedXMLSchema(root.Content[0], nil, "/*/ListPoliciesResult/PolicyNames/member")
	scalar, ok := selected.(interface{ ScalarColumn() string })
	if !ok || scalar.ScalarColumn() != "member" || !reflect.DeepEqual(selected.Properties(), []string{"member"}) {
		t.Fatalf("schema=%#v", selected)
	}
	value, ok := selected.Property("member")
	if !ok || value.(interface{ Type() string }).Type() != "string" {
		t.Fatalf("member schema=%#v", value)
	}
}
