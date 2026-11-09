package omnisdk_test

import (
	"fmt"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/docparse/dsl/gotemplate"
	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
)

var ordinarySDKPlan omnisdk.Plan

func BenchmarkOrdinarySDKPlan(b *testing.B) {
	for _, shape := range []string{"single", "join", "parallel", "google"} {
		b.Run(shape, func(b *testing.B) {
			address := "stackql_unstable_widgets.things.widgets"
			registry := authRegistry
			if shape == "google" {
				registry = "testdata/ordinary"
				address = "stackql_unstable_google.things.widgets"
			}
			nodes := []omnisdk.Node{omnisdk.NewNode("first", address, map[string]string{"id": "one"})}
			var wirings []omnisdk.Wiring
			if shape == "join" {
				nodes = append(nodes, omnisdk.NewNode("second", address, nil))
				wirings = []omnisdk.Wiring{omnisdk.NewWiring("second", []omnisdk.Inbound{omnisdk.NewInbound("first", "id", "parent")}, gotemplate.TypeJSON1, `{"id":"{{ .parent }}"}`, "id")}
			}
			if shape == "parallel" {
				for index := 1; index < 8; index++ {
					nodes = append(nodes, omnisdk.NewNode(fmt.Sprintf("read%d", index), address, map[string]string{"id": fmt.Sprint(index)}))
				}
			}
			graph, err := omnisdk.NewGraph(nodes, wirings)
			if err != nil {
				b.Fatal(err)
			}
			args := omnisdk.Args{Endpoint: "http://127.0.0.1:1"}
			if _, err := omnisdk.NewGraphQuery(registry, graph, args); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for index := 0; index < b.N; index++ {
				p, err := omnisdk.NewGraphQuery(registry, graph, args)
				if err != nil {
					b.Fatal(err)
				}
				ordinarySDKPlan = p
			}
		})
	}
}
