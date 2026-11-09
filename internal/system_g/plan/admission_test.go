package plan_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stackql-labs/omnisdk/internal/system_g/admit"
	"github.com/stackql-labs/omnisdk/internal/system_g/bind"
	"github.com/stackql-labs/omnisdk/internal/system_g/exchange/docx"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/plan"
	"github.com/stackql-labs/omnisdk/internal/system_g/schedule"
	"github.com/stackql-labs/omnisdk/pkg/docparse/dsl"
	language "github.com/stackql-labs/omnisdk/pkg/docparse/dsl/starlark"
)

type mockObservation func(context.Context) (map[string]any, error)

func (f mockObservation) Open(ctx context.Context) facade.Records {
	return &mockObservationRows{ctx: ctx, read: f}
}

type mockObservationRows struct {
	ctx  context.Context
	read mockObservation
	row  map[string]any
	err  error
	done bool
}

func (r *mockObservationRows) Next(ctx context.Context) bool {
	if r.done {
		return false
	}
	r.done = true
	r.row, r.err = r.read(ctx)
	return r.row != nil
}
func (r *mockObservationRows) Record() facade.Record { return bind.NewDocRecord(r.row) }
func (r *mockObservationRows) Err() error            { return r.err }
func (r *mockObservationRows) Close() error          { return nil }

// Mocked create -> {operation, detail} -> admission -> update. The losing read
// never returns on its own, proving the update does not wait for ALL sources.
func TestAdmissionRacesSufficientObservations(t *testing.T) {
	for _, winner := range []string{"operation", "detail", "either"} {
		t.Run(winner, func(t *testing.T) {
			reg, _ := dsl.NewRegistry(language.New())
			transform, err := docx.InboundTransform(reg, dsl.Transform{Type: "starlark", Body: `{"id": inbox["id"]}`, Admission: &dsl.Program{Type: "starlark", Body: `(edges["operation"].present and edges["operation"].value == "DONE") or (edges["detail"].present and edges["detail"].value == "RUNNING")`}})
			if err != nil {
				t.Fatal(err)
			}
			var updates, cancelled atomic.Int32
			create := plan.NewExchangeSpec("create", nil, []string{"id"}, func(map[string]any) facade.Operator {
				return mockObservation(func(context.Context) (map[string]any, error) { return map[string]any{"id": "db-1"}, nil })
			}, nil)
			observation := func(name, value string) plan.ExchangeSpec {
				return plan.WithReadOnly(plan.NewExchangeSpec(name, []string{"id"}, []string{name + "_state"}, func(inputs map[string]any) facade.Operator {
					return mockObservation(func(ctx context.Context) (map[string]any, error) {
						if inputs["id"] != "db-1" {
							return nil, fmt.Errorf("lost create handle")
						}
						if name != winner && winner != "either" {
							<-ctx.Done()
							cancelled.Add(1)
							return nil, ctx.Err()
						}
						return map[string]any{name + "_state": value}, nil
					})
				}, nil))
			}
			update := plan.WithInbound(plan.NewExchangeSpec("update", []string{"id", "operation", "detail"}, []string{"updated"}, func(inputs map[string]any) facade.Operator {
				return mockObservation(func(context.Context) (map[string]any, error) {
					updates.Add(1)
					return map[string]any{"updated": inputs["id"]}, nil
				})
			}, nil), transform)
			p := plan.NewPlan([]plan.ExchangeSpec{create, observation("operation", "DONE"), observation("detail", "RUNNING"), update}, []plan.BetaEdge{
				plan.NewBetaEdge("create", "operation", "id", "id"), plan.NewBetaEdge("create", "detail", "id", "id"), plan.NewBetaEdge("create", "update", "id", "id"), plan.NewBetaEdge("operation", "update", "operation_state", "operation"), plan.NewBetaEdge("detail", "update", "detail_state", "detail"),
			}, nil, nil, nil, nil)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if winner == "either" {
				ctx = schedule.WithLimiter(ctx, admit.NewSemaphore(1))
				ctx = schedule.WithSectionLimiter(ctx, admit.NewSemaphore(1))
			}
			rows := plan.ComposeRows(0, p).Open(ctx)
			defer rows.Close()
			count := 0
			for rows.Next(ctx) {
				count++
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if count != 1 || updates.Load() != 1 || (winner != "either" && cancelled.Load() != 1) {
				t.Fatalf("rows=%d updates=%d cancelled=%d", count, updates.Load(), cancelled.Load())
			}
		})
	}
}

func TestAdmissionDistinguishesEmptyFailedAndNull(t *testing.T) {
	for _, test := range []struct {
		name, predicate string
		row             map[string]any
		failure         error
		want            bool
	}{
		{"empty", `edges["observation"].done and not edges["observation"].present`, nil, nil, true},
		{"null", `edges["observation"].present and edges["observation"].value == None`, map[string]any{"value": nil}, nil, true},
		{"failed", `edges["observation"].done and edges["observation"].error != None`, nil, fmt.Errorf("operation handle expired"), true},
		{"insufficient", `edges["observation"].present`, nil, nil, false},
		{"non-boolean", `"yes"`, nil, nil, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			registry, _ := dsl.NewRegistry(language.New())
			transform, err := docx.InboundTransform(registry, dsl.Transform{Admission: &dsl.Program{Type: "starlark", Body: test.predicate}})
			if err != nil {
				t.Fatal(err)
			}
			source := plan.WithReadOnly(plan.NewExchangeSpec("source", nil, []string{"value"}, func(map[string]any) facade.Operator {
				return mockObservation(func(context.Context) (map[string]any, error) { return test.row, test.failure })
			}, nil))
			calls := 0
			consumer := plan.WithInbound(plan.NewExchangeSpec("consumer", []string{"observation"}, nil, func(map[string]any) facade.Operator {
				return mockObservation(func(context.Context) (map[string]any, error) { calls++; return map[string]any{}, nil })
			}, nil), transform)
			p := plan.NewPlan([]plan.ExchangeSpec{source, consumer}, []plan.BetaEdge{plan.NewBetaEdge("source", "consumer", "value", "observation")}, nil, nil, nil, nil)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			rows := plan.ComposeRows(0, p).Open(ctx)
			defer rows.Close()
			for rows.Next(ctx) {
			}
			if test.want {
				if rows.Err() != nil || calls != 1 {
					t.Fatalf("calls=%d err=%v", calls, rows.Err())
				}
			} else if rows.Err() == nil || calls != 0 {
				t.Fatalf("calls=%d err=%v", calls, rows.Err())
			}
		})
	}
}
