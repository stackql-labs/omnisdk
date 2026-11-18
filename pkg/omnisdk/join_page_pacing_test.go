package omnisdk_test

import (
	"context"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
	"github.com/stackql-labs/omnisdk/pkg/query"
)

func TestMultiJoinHeapShrinksBetweenPages(t *testing.T) {
	for _, workload := range []struct {
		name           string
		middle, leaves int
	}{
		{"lookup-chain", 1, 1},
		{"fanout-chain", 2, 3},
	} {
		t.Run(workload.name, func(t *testing.T) {
			const pages, pageRows = 4, 64
			const minShrink = pageRows * joinPayloadBytes / 2
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			requested := make(chan struct{})
			deliver := make(chan struct{})
			filler := strings.Repeat("x", joinPayloadBytes-8)
			columns := map[string]omnisdk.ColumnType{
				"key": {Type: "string"}, "seq": {Type: "integer"}, "payload": {Type: "string"},
			}
			driver, err := omnisdk.NewRelation("local.paged", nil, columns,
				func(ctx context.Context, _ map[string]any) (omnisdk.Rows, error) {
					var next int
					return &generatedJoinRows{
						count: pages * pageRows, key: strconv.Itoa,
						payload: func(i int) string { return filler + fmt.Sprintf("%08x", i) },
						advance: func() bool {
							atBoundary := next%pageRows == 0
							next++
							if !atBoundary {
								return true
							}
							select {
							case requested <- struct{}{}:
							case <-ctx.Done():
								return false
							}
							// Simulate the wait for the provider's next page.
							select {
							case <-deliver:
								return true
							case <-ctx.Done():
								return false
							}
						},
					}, nil
				})
			if err != nil {
				t.Fatal(err)
			}
			lookup := func(address string, fanout int) omnisdk.Relation {
				t.Helper()
				r, err := omnisdk.NewRelation(address, []omnisdk.Param{{Name: "parent", Required: true}}, columns,
					func(_ context.Context, inputs map[string]any) (omnisdk.Rows, error) {
						parent, err := strconv.Atoi(fmt.Sprint(inputs["parent"]))
						if err != nil || parent < 1 {
							return nil, fmt.Errorf("invalid parent key %v", inputs["parent"])
						}
						return &generatedJoinRows{count: fanout,
							key:     func(i int) string { return strconv.Itoa((parent-1)*fanout + i) },
							payload: func(int) string { return "" },
						}, nil
					})
				if err != nil {
					t.Fatal(err)
				}
				return r
			}
			mid, leaf := lookup("local.middle", workload.middle), lookup("local.leaf", workload.leaves)
			q, err := query.New([]query.Join{
				query.NewJoin(query.NewResource("d", driver.Address()), query.Base),
				query.NewJoin(query.NewResource("m", mid.Address()), query.Inner,
					query.NewEq(query.NewColumn("m", "parent"), query.NewColumn("d", "key"))),
				query.NewJoin(query.NewResource("l", leaf.Address()), query.Inner,
					query.NewEq(query.NewColumn("l", "parent"), query.NewColumn("m", "key"))),
			}, nil, []query.Output{
				query.NewOutput("driver", query.NewColumn("d", "seq")),
				query.NewOutput("middle", query.NewColumn("m", "seq")),
				query.NewOutput("leaf_key", query.NewColumn("l", "key")),
				query.NewOutput("payload", query.NewColumn("d", "payload")),
			})
			if err != nil {
				t.Fatal(err)
			}
			res, err := omnisdk.Resolve(q, map[string]omnisdk.Table{"d": driver, "m": mid, "l": leaf})
			if err != nil {
				t.Fatal(err)
			}
			pairsPerDriver := workload.middle * workload.leaves
			pageOutput := pageRows * pairsPerDriver
			plan, err := omnisdk.NewGraphQuery("unused", res.Graph(), omnisdk.Args{
				Relations: map[string]omnisdk.Relation{driver.Address(): driver, mid.Address(): mid, leaf.Address(): leaf},
				// One simulated page may fill the pipeline before draining.
				Tuning: omnisdk.Tuning{Parallelism: 8, RowsAhead: pageOutput, PagesAhead: 1},
			})
			if err != nil {
				t.Fatal(err)
			}
			rows, err := plan.Open(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			awaitRequest := func(page int) {
				t.Helper()
				select {
				case <-requested:
				case <-ctx.Done():
					t.Fatalf("waiting for page %d: %v", page, ctx.Err())
				}
			}
			heap := func() int64 {
				runtime.GC()
				var m runtime.MemStats
				runtime.ReadMemStats(&m)
				return int64(m.HeapAlloc)
			}

			awaitRequest(1)
			for page := range pages {
				select {
				case deliver <- struct{}{}:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				var queued int64
				if page+1 < pages {
					awaitRequest(page + 2)
					queued = heap()
				}
				seen := make([]bool, pageOutput)
				for range pageOutput {
					if !rows.Next() {
						t.Fatalf("page %d ended early: %v", page+1, rows.Err())
					}
					row := rows.Row()
					driverID, _ := strconv.Atoi(fmt.Sprint(row["driver"]))
					midID, _ := strconv.Atoi(fmt.Sprint(row["middle"]))
					leafKey, _ := strconv.Atoi(fmt.Sprint(row["leaf_key"]))
					payload, _ := row["payload"].(string)
					firstKey := page*pageOutput + 1
					midKey := (driverID-1)*workload.middle + midID
					if driverID <= page*pageRows || driverID > (page+1)*pageRows ||
						midID < 1 || midID > workload.middle ||
						leafKey <= (midKey-1)*workload.leaves || leafKey > midKey*workload.leaves ||
						len(payload) != joinPayloadBytes || payload[joinPayloadBytes-8:] != fmt.Sprintf("%08x", driverID) {
						t.Fatalf("invalid joined row in page %d: driver=%d middle=%d leaf key=%d", page+1, driverID, midID, leafKey)
					}
					if seen[leafKey-firstKey] {
						t.Fatalf("duplicate leaf key %d", leafKey)
					}
					seen[leafKey-firstKey] = true
				}
				if page+1 < pages {
					shrink := queued - heap()
					t.Logf("page %d drained while the next page waited: live heap shrank by %d bytes", page+1, shrink)
					if shrink < minShrink {
						t.Errorf("page %d: heap shrank by %d bytes, want at least %d", page+1, shrink, minShrink)
					}
				}
			}
			if rows.Next() || rows.Err() != nil {
				t.Fatalf("unexpected rows or error after final page: %v", rows.Err())
			}
		})
	}
}
