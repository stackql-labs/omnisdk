package plan

import (
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stackql-labs/omnisdk/internal/system_g/bind"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/termination"
)

// statuses is a source emitting one row per status.
func statuses(vs ...string) ExchangeSpec {
	return rowsOf("src", nil, nil, func(map[string]any) []map[string]any {
		var out []map[string]any
		for _, v := range vs {
			out = append(out, map[string]any{"status": v})
		}
		return out
	})
}

// byStatus chooses "done" for DONE, "failed" for FAILED, and "pending" otherwise.
func byStatus(name string) ExchangeSpec {
	return NewBranchSpec(name, []string{"s"}, func(b map[string]any) (string, error) {
		switch b["s"] {
		case "DONE":
			return "done", nil
		case "FAILED":
			return "failed", nil
		}
		return "pending", nil
	})
}

// marker is an exchange that adds one column, counting its runs.
func marker(name, col string, calls *atomic.Int64) ExchangeSpec {
	return rowsOf(name, nil, calls, func(map[string]any) []map[string]any {
		return []map[string]any{{col: name}}
	})
}

func gateTo(branch, arm, to string) AlphaEdge {
	return NewAlphaEdge(branch, to, bind.NewGateEdge(arm))
}

func shape(rows []map[string]any, cols ...string) string {
	var out []string
	for _, r := range rows {
		var parts []string
		for _, c := range cols {
			if v, ok := r[c]; ok {
				parts = append(parts, c+"="+fmt.Sprint(v))
			}
		}
		out = append(out, strings.Join(parts, " "))
	}
	slices.Sort(out)
	return strings.Join(out, " | ")
}

// Exactly one arm runs per row, and a row whose arm leads nowhere passes through untouched.
func TestBranchRunsOneArm(t *testing.T) {
	var onDone, onPending atomic.Int64
	p := NewPlan(
		[]ExchangeSpec{statuses("DONE", "RUNNING", "FAILED"), byStatus("br"), marker("get", "detail", &onDone), marker("wait", "again", &onPending)},
		[]BetaEdge{NewBetaEdge("src", "br", "status", "s")},
		[]AlphaEdge{gateTo("br", "done", "get"), gateTo("br", "pending", "wait")},
		nil, nil, nil)
	rows, err := collect(t, p)
	if err != nil {
		t.Fatal(err)
	}
	want := "status=DONE detail=get | status=FAILED | status=RUNNING again=wait"
	if got := shape(rows, "status", "detail", "again"); got != want {
		t.Errorf("rows = %s\nwant   %s", got, want)
	}
	if onDone.Load() != 1 || onPending.Load() != 1 {
		t.Errorf("runs: done %d, pending %d; want 1 each", onDone.Load(), onPending.Load())
	}
}

// A node several arms lead to runs when any of them is chosen.
func TestBranchArmsRejoin(t *testing.T) {
	var calls atomic.Int64
	p := NewPlan(
		[]ExchangeSpec{statuses("DONE", "RUNNING", "FAILED"), byStatus("br"), marker("report", "reported", &calls)},
		[]BetaEdge{NewBetaEdge("src", "br", "status", "s")},
		[]AlphaEdge{gateTo("br", "done", "report"), gateTo("br", "failed", "report")},
		nil, nil, nil)
	rows, err := collect(t, p)
	if err != nil {
		t.Fatal(err)
	}
	if got := shape(rows, "status", "reported"); got != "status=DONE reported=report | status=FAILED reported=report | status=RUNNING" {
		t.Errorf("rows = %s", got)
	}
	if calls.Load() != 2 {
		t.Errorf("report ran %d times, want 2", calls.Load())
	}
}

// An arm no row chooses never runs its node.
func TestDeadArmNeverRuns(t *testing.T) {
	var calls atomic.Int64
	p := NewPlan(
		[]ExchangeSpec{statuses("RUNNING", "RUNNING"), byStatus("br"), marker("get", "detail", &calls)},
		[]BetaEdge{NewBetaEdge("src", "br", "status", "s")},
		[]AlphaEdge{gateTo("br", "done", "get")},
		nil, nil, nil)
	rows, err := collect(t, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || calls.Load() != 0 {
		t.Errorf("rows = %v, runs = %d; want both rows through and no run", rows, calls.Load())
	}
}

// A poll loop exits through a branch: the poll repeats while the operation is pending, and the
// detail is read once it is done.
func TestPollLoopExitsThroughABranch(t *testing.T) {
	var polls atomic.Int64
	poll := rowsOf("poll", []string{"op"}, &polls, func(b map[string]any) []map[string]any {
		n := polls.Load() // the third poll finds it done
		status := "RUNNING"
		if n >= 3 {
			status = "DONE"
		}
		return []map[string]any{{"status": status, "seen": n}}
	})
	var gets atomic.Int64
	p := WithTerminations(NewPlan(
		[]ExchangeSpec{poll, byStatus("br"), marker("get", "detail", &gets)},
		[]BetaEdge{NewBetaEdge("", "poll", "operation", "op"), NewBetaEdge("poll", "br", "status", "s")},
		[]AlphaEdge{gateTo("br", "pending", "poll"), gateTo("br", "done", "get")},
		map[string]any{"operation": "op-1"}, nil, nil),
		map[string]facade.TerminationSpec{"poll": termination.Rounds(20)})
	rows, err := collect(t, p)
	if err != nil {
		t.Fatal(err)
	}
	if polls.Load() != 3 {
		t.Errorf("polled %d times, want 3", polls.Load())
	}
	if gets.Load() != 1 {
		t.Errorf("detail read %d times, want once", gets.Load())
	}
	done := 0
	for _, r := range rows {
		if r["detail"] == "get" {
			done++
		}
	}
	if done != 1 {
		t.Errorf("rows = %v, want one with the detail", rows)
	}
}
