package omnisdk

import (
	"fmt"
	"maps"

	"github.com/stackql-labs/omnisdk/pkg/query"
)

// Statement is what a caller asks for, before any of it is bound to a method. Every kind goes
// through the same analysis, so a resource is acted on the same way whichever kind asked.
type Statement interface{ statement() }

type graphStatement struct {
	g       Graph
	managed []Managed
}

type desiredStatement struct{ resources []ManagedResource }

type sqlStatement struct {
	q      query.Unresolved
	tables map[string]Table
}

func (graphStatement) statement()   {}
func (desiredStatement) statement() {}
func (sqlStatement) statement()     {}

// GraphStatement is a graph of reads and writes. managed names the mutation nodes a ledger records,
// and is only valid under NewLedger.
func GraphStatement(g Graph, managed ...Managed) Statement {
	return graphStatement{g: g, managed: managed}
}

// SQLStatement is a parsed query, with the table each of its aliases names. It resolves to a graph
// in Args.Dialect, and runs as that graph does; its constant bindings are Params for the run, over
// any of the same name in Args.Params.
func SQLStatement(q query.Unresolved, tables map[string]Table) Statement {
	return sqlStatement{q: q, tables: tables}
}

// DesiredState is a set of resources to converge on. It needs NewLedger: converging is deciding
// create, update or nothing from what the ledger last recorded.
func DesiredState(resources []ManagedResource) Statement {
	return desiredStatement{resources: resources}
}

// WALConfig says whether, and where, writes are recorded before they are sent. Every value it holds
// is scope and required: none is defaulted.
type WALConfig interface{ wal() }

type noWAL struct{}

type journalWAL struct{ state, runID string }

type ledgerWAL struct{ collection, state, runID string }

func (noWAL) wal()      {}
func (journalWAL) wal() {}
func (ledgerWAL) wal()  {}

// NoWAL records nothing: each write is sent as it is reached, and a failed run leaves what it did.
func NoWAL() WALConfig { return noWAL{} }

// NewJournal records each write's intent in runID's journal under stateDir before it is sent. Nothing
// is leased: two runs may write at once.
func NewJournal(stateDir, runID string) WALConfig { return journalWAL{state: stateDir, runID: runID} }

// NewLedger records each write per key in collection's ledger under stateDir, as well as in runID's
// journal, holds the collection for the run, and compensates a failed run.
func NewLedger(collection, stateDir, runID string) WALConfig {
	return ledgerWAL{collection: collection, state: stateDir, runID: runID}
}

// Prepare plans s against the documents under registry, recording writes as w says. Opening the
// returned Plan runs it.
//
// It is the one entry point: the older constructors are each one kind of statement under one WAL
// configuration. WAL configuration is w's alone, so args.Journal must be unset.
func Prepare(registry string, s Statement, w WALConfig, args Args) (Plan, error) {
	if args.Journal != nil {
		return nil, fmt.Errorf("omnisdk: Prepare takes its journal from WALConfig; leave Args.Journal unset")
	}
	if err := checkWAL(w); err != nil {
		return nil, err
	}
	switch st := s.(type) {
	case graphStatement:
		return prepareGraph(registry, st, w, args)
	case sqlStatement:
		res, err := ResolveIn(st.q, st.tables, args.Dialect)
		if err != nil {
			return nil, err
		}
		params := maps.Clone(args.Params)
		if params == nil {
			params = map[string]string{}
		}
		maps.Copy(params, res.Params())
		args.Params = params
		return prepareGraph(registry, graphStatement{g: res.Graph()}, w, args)
	case destroyStatement:
		l, ok := w.(ledgerWAL)
		if !ok {
			return nil, fmt.Errorf("omnisdk: destroy needs NewLedger: the ledger is what says what the collection holds")
		}
		return &destroyPlan{registry: registry, name: l.collection, state: l.state, runID: l.runID, args: args}, nil
	case desiredStatement:
		l, ok := w.(ledgerWAL)
		if !ok {
			return nil, fmt.Errorf("omnisdk: desired state needs NewLedger: converging is decided from what the ledger last recorded")
		}
		return converge(registry, l.collection, l.state, l.runID, st.resources, args)
	case nil:
		return nil, fmt.Errorf("omnisdk: no statement")
	}
	return nil, fmt.Errorf("omnisdk: unknown statement %T", s)
}

func prepareGraph(registry string, st graphStatement, w WALConfig, args Args) (Plan, error) {
	switch c := w.(type) {
	case ledgerWAL:
		return ConvergeGraph(registry, c.collection, c.state, c.runID, st.g, st.managed, args)
	case journalWAL:
		args.Journal = &Journal{State: c.state, RunID: c.runID}
	}
	if len(st.managed) > 0 {
		return nil, fmt.Errorf("omnisdk: managed nodes need NewLedger: a ledger is what records them")
	}
	return NewGraphQuery(registry, st.g, args)
}

// checkWAL refuses a configuration missing any of its scope.
func checkWAL(w WALConfig) error {
	switch c := w.(type) {
	case noWAL:
		return nil
	case journalWAL:
		if c.state == "" || c.runID == "" {
			return fmt.Errorf("omnisdk: NewJournal needs a state directory and a run id")
		}
		return nil
	case ledgerWAL:
		if c.collection == "" || c.state == "" || c.runID == "" {
			return fmt.Errorf("omnisdk: NewLedger needs a collection, a state directory and a run id")
		}
		return nil
	case nil:
		return fmt.Errorf("omnisdk: no WAL configuration; NoWAL says to record nothing")
	}
	return fmt.Errorf("omnisdk: unknown WAL configuration %T", w)
}
