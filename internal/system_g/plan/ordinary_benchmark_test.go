package plan_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stackql-labs/omnisdk/internal/system_g/bind"
	_ "github.com/stackql-labs/omnisdk/internal/system_g/exchange/docx"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/plan"
)

var ordinaryOperator facade.Operator

// Deliberately uses only pre-admission APIs, so the identical workload can be
// compiled against the parent implementation in the baseline checkout.
func ordinaryBenchmarkPlan(shape string) plan.Plan {
	count := 1
	if shape != "single" {
		count = 8
	}
	var specs []plan.ExchangeSpec
	var edges []plan.BetaEdge
	for index := 0; index < count; index++ {
		name := fmt.Sprintf("read%d", index)
		output := fmt.Sprintf("value%d", index)
		var inputs []string
		if shape == "chain" && index > 0 {
			inputs = []string{"parent"}
			edges = append(edges, plan.NewBetaEdge(fmt.Sprintf("read%d", index-1), name, fmt.Sprintf("value%d", index-1), "parent"))
		}
		specs = append(specs, plan.NewExchangeSpec(name, inputs, []string{output}, func(bound map[string]any) facade.Operator {
			value := any(1)
			if len(inputs) > 0 {
				value = bound["parent"]
			}
			return ordinarySeed{rec: bind.NewDocRecord(map[string]any{output: value})}
		}, bind.NewTupleFlatten()))
	}
	return plan.NewPlan(specs, edges, nil, nil, nil, nil)
}
func BenchmarkOrdinaryCompose(b *testing.B) {
	for _, shape := range []string{"single", "chain", "parallel"} {
		b.Run(shape, func(b *testing.B) {
			p := ordinaryBenchmarkPlan(shape)
			b.ReportAllocs()
			b.ResetTimer()
			for index := 0; index < b.N; index++ {
				operator := plan.ComposeRows(0, p)
				ordinaryOperator = operator
			}
		})
	}
}
func BenchmarkOrdinaryExecute(b *testing.B) {
	for _, shape := range []string{"single", "chain", "parallel"} {
		b.Run(shape, func(b *testing.B) {
			operator := plan.ComposeRows(0, ordinaryBenchmarkPlan(shape))
			ctx := context.Background()
			b.ReportAllocs()
			b.ResetTimer()
			for index := 0; index < b.N; index++ {
				rows := operator.Open(ctx)
				count := 0
				for rows.Next(ctx) {
					count++
				}
				err := rows.Err()
				rows.Close()
				if err != nil || count != 1 {
					b.Fatalf("rows=%d err=%v", count, err)
				}
			}
		})
	}
}

type ordinarySeed struct{ rec facade.Record }

func (s ordinarySeed) Open(context.Context) facade.Records { return &ordinarySeedRows{record: s.rec} }

type ordinarySeedRows struct {
	record facade.Record
	done   bool
}

func (r *ordinarySeedRows) Next(context.Context) bool {
	if r.done {
		return false
	}
	r.done = true
	return true
}
func (r *ordinarySeedRows) Record() facade.Record { return r.record }
func (r *ordinarySeedRows) Err() error            { return nil }
func (r *ordinarySeedRows) Close() error          { return nil }
