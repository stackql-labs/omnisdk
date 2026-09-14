// Package journal implements the ordered forward log behind facade.Journal.
//
// One append-only file per run, written ahead of each wire effect. Unwind reverses it. The order
// has to be durable rather than reconstructed from the plan graph, because a crashed run is torn
// down by a different process with nothing in memory — and it needs no more machinery than the
// log itself, which is what the durable-execution engines do.
//
// facade.SagaLog is left alone: this is a separate contract, so nothing on the query path changes.
package journal

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

// line is one journal record. Fields carry storage names so the accessors can carry the contract
// names; the JSON tags are the durable form and must not drift.
type line struct {
	SeqNo     int              `json:"seq"`
	KeyName   facade.LedgerKey `json:"key"`
	XchgName  string           `json:"exchange"`
	FormClass facade.FormClass `json:"form"`
	PriorPlan []byte           `json:"prior,omitempty"`
}

func (l line) Seq() int               { return l.SeqNo }
func (l line) Key() facade.LedgerKey  { return l.KeyName }
func (l line) Exchange() string       { return l.XchgName }
func (l line) Form() facade.FormClass { return l.FormClass }
func (l line) Prior() []byte          { return l.PriorPlan }

type files struct{ root string }

// NewFiles returns Journals backed by one file per run under root.
func NewFiles(root string) (facade.Journals, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("journal root: %w", err)
	}
	return &files{root: root}, nil
}

func (f *files) For(_ context.Context, runID string) (facade.Journal, error) {
	if runID == "" || strings.ContainsAny(runID, `/\.`) {
		return nil, fmt.Errorf("journal: illegal run id %q", runID)
	}
	return &runJournal{path: filepath.Join(f.root, runID+".jsonl")}, nil
}

// runJournal appends under a mutex and O_APPEND, and fsyncs before returning: the record must be
// on disk before the effect it describes is attempted, or a crash leaves an effect nothing knows
// to compensate.
type runJournal struct {
	mu   sync.Mutex
	path string
	seq  int
}

func (r *runJournal) Append(_ context.Context, k facade.LedgerKey, exchange string, form facade.FormClass, prior []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.seq == 0 {
		existing, err := r.read()
		if err != nil {
			return 0, err
		}
		r.seq = len(existing)
	}
	r.seq++
	b, err := json.Marshal(line{SeqNo: r.seq, KeyName: k, XchgName: exchange, FormClass: form, PriorPlan: prior})
	if err != nil {
		return 0, fmt.Errorf("journal: encode: %w", err)
	}
	fh, err := os.OpenFile(r.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, fmt.Errorf("journal: open %s: %w", r.path, err)
	}
	defer fh.Close()
	if _, err := fh.Write(append(b, '\n')); err != nil {
		return 0, fmt.Errorf("journal: append %s: %w", r.path, err)
	}
	if err := fh.Sync(); err != nil {
		return 0, fmt.Errorf("journal: sync %s: %w", r.path, err)
	}
	return r.seq, nil
}

func (r *runJournal) read() ([]line, error) {
	fh, err := os.Open(r.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("journal: open %s: %w", r.path, err)
	}
	defer fh.Close()
	var out []line
	sc := bufio.NewScanner(fh)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var l line
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			return nil, fmt.Errorf("journal: decode %s: %w", r.path, err)
		}
		out = append(out, l)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("journal: read %s: %w", r.path, err)
	}
	return out, nil
}

func (r *runJournal) Records(_ context.Context) ([]facade.JournalRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ls, err := r.read()
	if err != nil {
		return nil, err
	}
	out := make([]facade.JournalRecord, len(ls))
	for i, l := range ls {
		out[i] = l
	}
	return out, nil
}
