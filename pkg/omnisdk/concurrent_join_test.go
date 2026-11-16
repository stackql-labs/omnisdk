package omnisdk_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
	"github.com/stackql-labs/omnisdk/pkg/query"
	"github.com/stackql-labs/omnisdk/public/authorization"
	"github.com/stackql-labs/omnisdk/public/principal"
)

func TestConcurrentHighVolumeJoins(t *testing.T) {
	if testing.Short() {
		t.Skip("runs 12 overlapping joins")
	}
	const child = "OMNISDK_CONCURRENT_JOIN_TEST"
	if os.Getenv(child) == "" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestConcurrentHighVolumeJoins$", "-test.v", "-test.timeout=30s")
		cmd.Env = append(os.Environ(), child+"=1")
		out, err := cmd.CombinedOutput()
		t.Logf("%s", out)
		if err != nil {
			t.Fatalf("concurrent join subprocess: %v", err)
		}
		return
	}

	const sessions, driving, matches = 12, 64, 16
	const rowsPerJoin = driving * matches
	const heapBudget = 24 << 20
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	directory, err := principal.NewMemoryDirectoryFromBytes([]byte(`{"principals":[{"id":"alice","identities":[{"issuer":"test","subject":"alice"}]},{"id":"bob","identities":[{"issuer":"test","subject":"bob"}]},{"id":"carol","identities":[{"issuer":"test","subject":"carol"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	names := []string{"alice", "bob", "carol"}
	var rules []authorization.Rule
	var private []omnisdk.Relation
	driver, err := omnisdk.NewRelation("local.driver", nil, map[string]omnisdk.ColumnType{"key": {Type: "string"}},
		func(context.Context, map[string]any) (omnisdk.Rows, error) {
			return &generatedJoinRows{count: driving, key: strconv.Itoa, payload: func(int) string { return "" }}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	for _, owner := range names {
		rules = append(rules,
			authorization.Rule{Subject: "user:" + owner, Resource: "relation/local.driver", Action: "read"},
			authorization.Rule{Subject: "user:" + owner, Resource: "relation/local." + owner, Action: "read"},
		)
		filler := strings.Repeat("x", joinPayloadBytes-len(owner)-8)
		relation, err := omnisdk.NewRelation("local."+owner, []omnisdk.Param{{Name: "owner_key", Required: true}},
			map[string]omnisdk.ColumnType{"seq": {Type: "integer"}, "payload": {Type: "string"}},
			func(_ context.Context, inputs map[string]any) (omnisdk.Rows, error) {
				key, err := strconv.Atoi(fmt.Sprint(inputs["owner_key"]))
				if err != nil || key < 1 || key > driving {
					return nil, fmt.Errorf("invalid lookup key %v", inputs["owner_key"])
				}
				return &generatedJoinRows{count: matches, key: func(int) string { return "" }, payload: func(i int) string {
					return owner + filler + fmt.Sprintf("%08x", (key-1)*matches+i)
				}}, nil
			})
		if err != nil {
			t.Fatal(err)
		}
		private = append(private, relation)
	}
	policy, err := authorization.NewCasbin(rules)
	if err != nil {
		t.Fatal(err)
	}
	makePlan := func(caller authorization.Session, target omnisdk.Relation) omnisdk.Plan {
		t.Helper()
		q, err := query.New([]query.Join{
			query.NewJoin(query.NewResource("d", driver.Address()), query.Base),
			query.NewJoin(query.NewResource("r", target.Address()), query.Inner,
				query.NewEq(query.NewColumn("d", "key"), query.NewColumn("r", "owner_key"))),
		}, nil, []query.Output{
			query.NewOutput("key", query.NewColumn("d", "key")),
			query.NewOutput("seq", query.NewColumn("r", "seq")),
			query.NewOutput("payload", query.NewColumn("r", "payload")),
		})
		if err != nil {
			t.Fatal(err)
		}
		res, err := omnisdk.Resolve(q, map[string]omnisdk.Table{"d": driver, "r": target})
		if err != nil {
			t.Fatal(err)
		}
		p, err := omnisdk.NewGraphQuery("unused", res.Graph(), omnisdk.Args{
			Caller: caller, RequireCaller: true,
			Relations: map[string]omnisdk.Relation{driver.Address(): driver, target.Address(): target},
			Tuning:    omnisdk.Tuning{Parallelism: 2, RowsAhead: 4, PagesAhead: 1},
		})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	var allowed, denied [sessions]omnisdk.Plan
	for i := range sessions {
		owner := i % len(names)
		identity, err := principal.NewIdentity("test", names[owner])
		if err != nil {
			t.Fatal(err)
		}
		caller, err := authorization.NewSession(ctx, identity, directory, policy)
		if err != nil {
			t.Fatal(err)
		}
		allowed[i] = makePlan(caller, private[owner])
		denied[i] = makePlan(caller, private[(owner+1)%len(names)])
	}

	heap := func() int64 {
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return int64(m.HeapAlloc)
	}
	base := heap()
	// Each barrier holds the consumers in flight while the executor runs
	// normally and applies its configured read-ahead.
	checkpoints := []int{1, 256, 768}
	arrived := make(chan struct{}, sessions)
	release := make([]chan struct{}, len(checkpoints))
	for i := range release {
		release[i] = make(chan struct{})
	}
	var workers sync.WaitGroup
	defer func() {
		cancel()
		workers.Wait()
	}()
	for i := range sessions {
		workers.Go(func() {
			workerCtx, stop := context.WithCancel(ctx)
			defer stop()
			fail := func(format string, args ...any) {
				t.Errorf("session %d: %s", i, fmt.Sprintf(format, args...))
				cancel()
			}
			blocked, err := denied[i].Open(workerCtx)
			if err == nil {
				if blocked.Next() {
					blocked.Close()
					fail("cross-principal join returned a row")
					return
				}
				err = blocked.Err()
				blocked.Close()
			}
			if !errors.Is(err, authorization.ErrDenied) {
				fail("cross-principal join returned %v, want access denied", err)
				return
			}

			rows, err := allowed[i].Open(workerCtx)
			if err != nil {
				fail("authorized join: %v", err)
				return
			}
			defer rows.Close()
			owner := names[i%len(names)]
			var seen [rowsPerJoin]bool
			count, checkpoint := 0, 0
			for rows.Next() {
				row := rows.Row()
				key, keyErr := strconv.Atoi(fmt.Sprint(row["key"]))
				seq, seqErr := strconv.Atoi(fmt.Sprint(row["seq"]))
				payload, _ := row["payload"].(string)
				if keyErr != nil || seqErr != nil || key < 1 || key > driving || seq < 1 || seq > matches {
					fail("invalid join pair at row %d", count)
					return
				}
				pair := (key-1)*matches + seq - 1
				if seen[pair] || len(payload) != joinPayloadBytes || !strings.HasPrefix(payload, owner) ||
					payload[joinPayloadBytes-8:] != fmt.Sprintf("%08x", pair+1) {
					fail("duplicate, corrupt, or foreign row at pair %d", pair)
					return
				}
				seen[pair] = true
				count++
				if checkpoint < len(checkpoints) && count == checkpoints[checkpoint] {
					arrived <- struct{}{}
					select {
					case <-release[checkpoint]:
					case <-ctx.Done():
						return
					}
					if i == 0 && checkpoint == 1 {
						stop()
						if rows.Next() || rows.Err() != nil {
							fail("cursor did not stop cleanly after cancellation: %v", rows.Err())
						}
						return
					}
					checkpoint++
				}
			}
			if count != rowsPerJoin || rows.Err() != nil {
				fail("drained %d of %d rows: %v", count, rowsPerJoin, rows.Err())
			}
		})
	}
	var peak int64
	for checkpoint := range checkpoints {
		expected := sessions
		if checkpoint == 2 {
			expected-- // session zero cancelled after the preceding sample
		}
		for range expected {
			select {
			case <-arrived:
			case <-ctx.Done():
				t.Fatalf("checkpoint %d: %v", checkpoint, ctx.Err())
			}
		}
		peak = max(peak, heap()-base)
		close(release[checkpoint])
	}
	workers.Wait()
	output := ((sessions-1)*rowsPerJoin + checkpoints[1]) * joinPayloadBytes
	t.Logf("verified %d payload bytes; peak sampled live heap growth %d bytes (limit %d)", output, peak, heapBudget)
	if peak > heapBudget {
		t.Fatalf("concurrent join live heap exceeds budget: %d > %d", peak, heapBudget)
	}
}
