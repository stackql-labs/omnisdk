package omnisdk_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
	"github.com/stackql-labs/omnisdk/pkg/query"
)

const joinPayloadBytes = 16 << 10

const joinHeapWorkingSetMultiplier = 2

type joinMemoryCase struct {
	name                 string
	form                 query.JoinForm
	partitions, matching int
	keyed                bool
	retain               bool
}

func TestJoinWorkingSetHeap(t *testing.T) {
	if testing.Short() {
		t.Skip("exercises large join inputs and output")
	}
	cases := []joinMemoryCase{
		{"left/small", query.Left, 16, 16, true, false},
		{"left/more-output", query.Left, 256, 16, true, false},
		{"left/wider", query.Left, 32, 128, true, false},
		{"inner/small", query.Inner, 16, 16, true, false},
		{"inner/more-output", query.Inner, 256, 16, true, false},
		{"inner/wider", query.Inner, 32, 128, true, false},
		{"hash/small", query.Listed, 16, 16, false, false},
		{"hash/more-output", query.Listed, 256, 16, false, false},
		{"hash/wider", query.Listed, 32, 128, false, false},
		{"cross/small", query.Cross, 16, 16, false, false},
		{"cross/more-output", query.Cross, 256, 16, false, false},
		{"cross/wider", query.Cross, 32, 128, false, false},
		{"left/retain-output", query.Left, 256, 16, true, true},
	}
	const scenario = "OMNISDK_JOIN_HEAP_CASE"
	if name := os.Getenv(scenario); name != "" {
		for _, c := range cases {
			if c.name == name {
				checkJoinWorkingSet(t, c)
				return
			}
		}
		t.Fatalf("unknown heap case %q", name)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestJoinWorkingSetHeap$", "-test.v")
			cmd.Env = append(os.Environ(), scenario+"="+c.name)
			out, err := cmd.CombinedOutput()
			t.Logf("%s", out)
			if c.retain {
				if err == nil || !strings.Contains(string(out), "join live heap exceeds budget") {
					t.Fatalf("retention control did not breach the heap budget: %v", err)
				}
			} else if err != nil {
				t.Fatalf("heap test subprocess: %v", err)
			}
		})
	}
}

type generatedJoinRows struct {
	count, next int
	key         func(int) string
	payload     func(int) string
	advance     func() bool
}

func (r *generatedJoinRows) Next() bool {
	r.next++
	return r.next <= r.count && (r.advance == nil || r.advance())
}

func (r *generatedJoinRows) Row() omnisdk.Row {
	return omnisdk.Row{"key": r.key(r.next), "seq": r.next, "payload": r.payload(r.next)}
}

func (*generatedJoinRows) Err() error   { return nil }
func (*generatedJoinRows) Close() error { return nil }

func checkJoinWorkingSet(t *testing.T, c joinMemoryCase) {
	filler := strings.Repeat("x", joinPayloadBytes-8)
	payload := func(i int) string {
		return filler + fmt.Sprintf("%08x", i)
	}
	columns := map[string]omnisdk.ColumnType{
		"key": {Type: "string"}, "seq": {Type: "integer"}, "payload": {Type: "string"},
	}
	// Admit one driving partition at a time within the same open query.
	permit := make(chan struct{}, 1)
	permit <- struct{}{}
	src, err := omnisdk.NewRelation("local.driver", nil, columns,
		func(ctx context.Context, _ map[string]any) (omnisdk.Rows, error) {
			key := func(int) string { return "shared" }
			if c.keyed {
				key = strconv.Itoa
			}
			return &generatedJoinRows{count: c.partitions, key: key, payload: payload, advance: func() bool {
				select {
				case <-permit:
					return true
				case <-ctx.Done():
					return false
				}
			}}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	var params []omnisdk.Param
	var on []query.Predicate
	if c.form != query.Cross {
		match := "key"
		if c.keyed {
			match = "owner"
			params = []omnisdk.Param{{Name: match, Required: true}}
		}
		on = append(on, query.NewEq(query.NewColumn("r", match), query.NewColumn("d", "key")))
	}
	right, err := omnisdk.NewRelation("local.matches", params, columns,
		func(_ context.Context, inputs map[string]any) (omnisdk.Rows, error) {
			offset := 0
			if c.keyed {
				owner, err := strconv.Atoi(fmt.Sprint(inputs["owner"]))
				if err != nil || owner < 1 || owner > c.partitions {
					return nil, fmt.Errorf("invalid lookup owner %v", inputs["owner"])
				}
				offset = (owner - 1) * c.matching
			}
			return &generatedJoinRows{count: c.matching, key: func(int) string { return "shared" },
				payload: func(i int) string { return payload(offset + i) }}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	q, err := query.New([]query.Join{
		query.NewJoin(query.NewResource("d", src.Address()), query.Base),
		query.NewJoin(query.NewResource("r", right.Address()), c.form, on...),
	}, nil, []query.Output{
		query.NewOutput("driver", query.NewColumn("d", "seq")),
		query.NewOutput("match", query.NewColumn("r", "seq")),
		query.NewOutput("driver_payload", query.NewColumn("d", "payload")),
		query.NewOutput("match_payload", query.NewColumn("r", "payload")),
	})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := omnisdk.Resolve(q, map[string]omnisdk.Table{"d": src, "r": right})
	if err != nil {
		t.Fatal(err)
	}
	p, err := omnisdk.NewGraphQuery("unused", resolved.Graph(), omnisdk.Args{
		Relations: map[string]omnisdk.Relation{src.Address(): src, right.Address(): right},
		Tuning:    omnisdk.Tuning{Parallelism: 2, RowsAhead: 4, PagesAhead: 1},
	})
	if err != nil {
		t.Fatal(err)
	}

	// One driving row plus its matching response, or one driving row plus
	// the shared replay input. The envelope also allows Go row/index objects,
	// allocator overhead and bounded pipeline buffers.
	working := int64(c.matching+1) * joinPayloadBytes
	budget := joinHeapWorkingSetMultiplier*working + (1 << 20)
	heap := func() int64 {
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return int64(m.HeapAlloc)
	}
	want := c.partitions * c.matching
	seen := make([]bool, want)
	base := heap()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	rows, err := p.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var count int
	var peak, outputBytes int64
	var retained []string
	for rows.Next() {
		row := rows.Row()
		driver, driverErr := strconv.Atoi(fmt.Sprint(row["driver"]))
		match, matchErr := strconv.Atoi(fmt.Sprint(row["match"]))
		if driverErr != nil || matchErr != nil || driver < 1 || driver > c.partitions || match < 1 || match > c.matching {
			t.Fatalf("unexpected output pair at row %d: driver=%v match=%v", count, row["driver"], row["match"])
		}
		pair := (driver-1)*c.matching + match - 1
		if seen[pair] {
			t.Fatalf("duplicate pair: driver=%d match=%d", driver, match)
		}
		seen[pair] = true
		matchID := match
		if c.keyed {
			matchID = pair + 1
		}
		left, lok := row["driver_payload"].(string)
		right, rok := row["match_payload"].(string)
		if !lok || !rok || len(left) != joinPayloadBytes || len(right) != joinPayloadBytes ||
			left[joinPayloadBytes-8:] != fmt.Sprintf("%08x", driver) ||
			right[joinPayloadBytes-8:] != fmt.Sprintf("%08x", matchID) {
			t.Fatalf("incorrect payload at row %d (driver %d, match %d)", count, driver, match)
		}
		if c.retain {
			retained = append(retained, right)
		}
		outputBytes += int64(len(left) + len(right))
		count++
		// Spread in-flight samples across the run, offset from the expected
		// partition boundaries.
		if count%(want/8) == c.matching/2 || count == want-1 {
			peak = max(peak, heap()-base)
		}
		if count%c.matching == 0 && count < want {
			permit <- struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("output count = %d, want %d", count, want)
	}
	runtime.KeepAlive(retained)
	t.Logf("rows=%d logical output=%d working payload=%d peak post-GC heap growth=%d budget=%d",
		count, outputBytes, working, peak, budget)
	if peak > budget {
		t.Fatalf("join live heap exceeds budget: %d > %d", peak, budget)
	}
	if !c.keyed && peak < int64(c.matching)*joinPayloadBytes*3/4 {
		t.Fatalf("heap samples did not capture the live replay input: peak %d, payload %d", peak, c.matching*joinPayloadBytes)
	}
}
