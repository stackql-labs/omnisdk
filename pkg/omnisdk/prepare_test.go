package omnisdk_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
	"github.com/stackql-labs/omnisdk/pkg/query"
)

func drain(t *testing.T, pl omnisdk.Plan) []omnisdk.Row {
	t.Helper()
	rows, err := pl.Open(context.Background())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer rows.Close()
	var out []omnisdk.Row
	for rows.Next() {
		out = append(out, rows.Row())
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// Under a ledger, a graph statement is a converging run: absent → create, then a rerun only reads.
func TestPrepareGraphUnderLedgerConverges(t *testing.T) {
	store := &widgetStore{items: map[string]map[string]any{}}
	srv := store.serve(t)
	defer srv.Close()
	state := t.TempDir()
	run := func(runID string) {
		nodes, wirings, br, gates, m := widgetResource("w1", map[string]any{"size": "L", "color": "red"})
		g, err := omnisdk.NewGraph(nodes, wirings)
		if err != nil {
			t.Fatal(err)
		}
		if g, err = omnisdk.WithBranch(g, br, gates...); err != nil {
			t.Fatal(err)
		}
		pl, err := omnisdk.Prepare(authRegistry, omnisdk.GraphStatement(g, m), omnisdk.NewLedger("demo", state, runID), omnisdk.Args{Endpoint: srv.URL})
		if err != nil {
			t.Fatalf("prepare: %v", err)
		}
		drain(t, pl)
	}
	run("r1")
	if got := strings.Join(store.took(), ","); got != "create" {
		t.Errorf("first run: calls %s, want a create", got)
	}
	run("r2")
	if got := strings.Join(store.took(), ","); got != "find" {
		t.Errorf("second run: calls %s, want only a read", got)
	}
}

// With no WAL a graph statement is a plain query; with a journal its writes are recorded first.
func TestPrepareGraphUnderNoWALAndJournal(t *testing.T) {
	store := &widgetStore{items: map[string]map[string]any{"w-9": {"id": "w-9", "size": "S"}}}
	srv := store.serve(t)
	defer srv.Close()

	read, err := omnisdk.NewGraph([]omnisdk.Node{omnisdk.NewNode("", widgets, map[string]string{"id": "w-9"})}, nil)
	if err != nil {
		t.Fatal(err)
	}
	pl, err := omnisdk.Prepare(authRegistry, omnisdk.GraphStatement(read), omnisdk.NoWAL(), omnisdk.Args{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if rows := drain(t, pl); len(rows) != 1 || rows[0]["size"] != "S" {
		t.Errorf("rows = %v, want the one widget", rows)
	}

	state := t.TempDir()
	write, err := omnisdk.NewGraph([]omnisdk.Node{
		omnisdk.NewMutationNode("", widgets, "insert", nil, nil, map[string]any{"size": "M"}),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	pl, err = omnisdk.Prepare(authRegistry, omnisdk.GraphStatement(write), omnisdk.NewJournal(state, "r1"), omnisdk.Args{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	drain(t, pl)
	entries, err := os.ReadDir(filepath.Join(state, "journal"))
	if err != nil || len(entries) == 0 {
		t.Errorf("journal = %v, %v; want the run's intent recorded", entries, err)
	}
}

// Missing scope, a second place for the journal, and a statement its WAL cannot carry are refused
// before anything runs.
func TestPrepareRefuses(t *testing.T) {
	g, err := omnisdk.NewGraph([]omnisdk.Node{omnisdk.NewNode("", widgets, nil)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, _, m := widgetResource("w1", map[string]any{"size": "L"})
	res := []omnisdk.ManagedResource{omnisdk.NewResource("k", "widgets", "things.widgets", []byte(`{}`), nil, nil, "", "", "id", "id", "")}
	for name, c := range map[string]struct {
		s    omnisdk.Statement
		w    omnisdk.WALConfig
		args omnisdk.Args
		want string
	}{
		"nil wal":             {omnisdk.GraphStatement(g), nil, omnisdk.Args{}, "no WAL configuration"},
		"journal without id":  {omnisdk.GraphStatement(g), omnisdk.NewJournal("/tmp/x", ""), omnisdk.Args{}, "run id"},
		"ledger without name": {omnisdk.GraphStatement(g), omnisdk.NewLedger("", "/tmp/x", "r"), omnisdk.Args{}, "collection"},
		"journal in args": {omnisdk.GraphStatement(g), omnisdk.NoWAL(),
			omnisdk.Args{Journal: &omnisdk.Journal{State: "/tmp/x", RunID: "r"}}, "Args.Journal"},
		"desired without ledger": {omnisdk.DesiredState(res), omnisdk.NewJournal("/tmp/x", "r"), omnisdk.Args{}, "needs NewLedger"},
		"managed without ledger": {omnisdk.GraphStatement(g, m), omnisdk.NoWAL(), omnisdk.Args{}, "need NewLedger"},
	} {
		if _, err := omnisdk.Prepare(authRegistry, c.s, c.w, c.args); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want one mentioning %q", name, err, c.want)
		}
	}
}

// SQL is one more statement kind: resolved to a graph, it runs as a graph statement does — a read
// with no WAL, and a write whose intent the journal records first.
func TestPrepareSQLStatement(t *testing.T) {
	store := &widgetStore{items: map[string]map[string]any{"w-9": {"id": "w-9", "size": "S"}}}
	srv := store.serve(t)
	defer srv.Close()

	read, err := omnisdk.DescribeTable(authRegistry, widgets)
	if err != nil {
		t.Fatal(err)
	}
	q, err := query.New(
		[]query.Join{query.NewJoin(query.NewResource("w", "widgets.things.widgets"), query.Base)},
		[]query.Predicate{query.NewEq(query.NewColumn("w", "id"), query.NewLiteral("w-9"))},
		[]query.Output{query.NewOutput("size", query.NewColumn("w", "size"))},
	)
	if err != nil {
		t.Fatal(err)
	}
	pl, err := omnisdk.Prepare(authRegistry, omnisdk.SQLStatement(q, map[string]omnisdk.Table{"w": read}), omnisdk.NoWAL(), omnisdk.Args{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("prepare select: %v", err)
	}
	if rows := drain(t, pl); len(rows) != 1 || rows[0]["size"] != "S" {
		t.Errorf("rows = %v, want the one widget's size", rows)
	}

	write, err := omnisdk.DescribeMutation(authRegistry, widgets, "insert")
	if err != nil {
		t.Fatal(err)
	}
	ins, err := query.NewMutation(query.NewInsert(query.NewResource("w", "widgets.things.widgets"),
		query.NewAssignment("size", query.NewLiteral("M"))), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	pl, err = omnisdk.Prepare(authRegistry, omnisdk.SQLStatement(ins, map[string]omnisdk.Table{"w": write}), omnisdk.NewJournal(state, "r1"), omnisdk.Args{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("prepare insert: %v", err)
	}
	drain(t, pl)
	if got := strings.Join(store.took(), ","); !strings.Contains(got, "create") {
		t.Errorf("calls = %s, want the insert sent", got)
	}
	if entries, err := os.ReadDir(filepath.Join(state, "journal")); err != nil || len(entries) == 0 {
		t.Errorf("journal = %v, %v; want the insert's intent recorded", entries, err)
	}
}
