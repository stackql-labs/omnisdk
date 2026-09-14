package ledger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

// The version lives in the *filename*, not in the file. rename is atomic but unconditional — two
// writers that both read v5 both land and the later silently wins — so it cannot implement
// compare-and-swap. link is atomic *and* exclusive, which makes it both the durable write and the
// compare: EEXIST is the conflict. This is the local analogue of If-Match on object storage.
const (
	verInfix  = ".v"
	verSuffix = ".json"
)

// state is the on-disk form of an entry. Deletion is a tombstone version rather than an unlink:
// retention is the irreversible decision, so nothing is ever removed or written over in place.
type state struct {
	Phase     facade.LedgerPhase `json:"phase"`
	Plan      []byte             `json:"plan,omitempty"`
	Proposed  []byte             `json:"proposed,omitempty"`
	Identity  []byte             `json:"identity,omitempty"`
	Tombstone bool               `json:"tombstone,omitempty"`
}

type fileLedger struct{ root string }

// NewFile returns a Ledger over a directory tree, one object per key. It is the v1 durable store,
// and the same interface covers commodity object storage, where the ETag replaces the filename
// version.
//
// Local disk only: O_EXCL and link are unreliable on NFSv3, so a network share is not a supported
// backing store.
func NewFile(root string) (facade.Ledger, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("ledger root: %w", err)
	}
	return &fileLedger{root: root}, nil
}

// paths resolves a key to the directory holding its versions and the filename stem within it.
// Keys are scope-prefixed with "/" and must not escape the root.
func (f *fileLedger) paths(k facade.LedgerKey) (dir, stem string, err error) {
	segs := strings.Split(string(k), "/")
	for _, s := range segs {
		if s == "" || s == "." || s == ".." || strings.ContainsAny(s, `\x00/\`) {
			return "", "", fmt.Errorf("ledger: illegal key segment %q in %q", s, k)
		}
	}
	stem = segs[len(segs)-1]
	dir = filepath.Join(append([]string{f.root}, segs[:len(segs)-1]...)...)
	return dir, stem, nil
}

// current reads the highest version present for a key. found reports logical existence: a
// tombstone is absent to callers while its version still governs what may be written next.
func (f *fileLedger) current(k facade.LedgerKey) (s state, ver uint64, found bool, err error) {
	dir, stem, err := f.paths(k)
	if err != nil {
		return state{}, 0, false, err
	}
	ents, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return state{}, 0, false, nil
	}
	if err != nil {
		return state{}, 0, false, fmt.Errorf("ledger read %s: %w", dir, err)
	}
	prefix := stem + verInfix
	for _, e := range ents {
		name := e.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, verSuffix) {
			continue
		}
		n, convErr := strconv.ParseUint(name[len(prefix):len(name)-len(verSuffix)], 10, 64)
		if convErr != nil || n <= ver {
			continue
		}
		ver = n
	}
	if ver == 0 {
		return state{}, 0, false, nil
	}
	b, err := os.ReadFile(f.versionPath(dir, stem, ver))
	if err != nil {
		return state{}, 0, false, fmt.Errorf("ledger read %s: %w", k, err)
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return state{}, 0, false, fmt.Errorf("ledger decode %s: %w", k, err)
	}
	return s, ver, !s.Tombstone, nil
}

func (f *fileLedger) versionPath(dir, stem string, n uint64) string {
	return filepath.Join(dir, stem+verInfix+strconv.FormatUint(n, 10)+verSuffix)
}

// commit writes s as the version after ver. The temp file is fsynced before it is linked, so a
// partially written file is never visible under a version name, and EEXIST on the link means a
// concurrent writer claimed the version first.
func (f *fileLedger) commit(k facade.LedgerKey, s state, ver uint64) error {
	dir, stem, err := f.paths(k)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("ledger mkdir %s: %w", dir, err)
	}
	b, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("ledger encode %s: %w", k, err)
	}
	tmp, err := os.CreateTemp(dir, stem+".tmp*")
	if err != nil {
		return fmt.Errorf("ledger temp %s: %w", k, err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("ledger write %s: %w", k, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("ledger sync %s: %w", k, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("ledger close %s: %w", k, err)
	}
	if err := os.Link(tmp.Name(), f.versionPath(dir, stem, ver+1)); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%w: %s v%d claimed", facade.ErrLedgerConflict, k, ver+1)
		}
		return fmt.Errorf("ledger link %s: %w", k, err)
	}
	return syncDir(dir)
}

// syncDir makes the new link itself durable; without it the file survives a crash but the
// directory entry naming it may not.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("ledger opendir %s: %w", dir, err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("ledger syncdir %s: %w", dir, err)
	}
	return nil
}

func (f *fileLedger) Get(_ context.Context, k facade.LedgerKey) (facade.LedgerEntry, facade.LedgerVersion, bool, error) {
	s, ver, found, err := f.current(k)
	if err != nil || !found {
		return nil, facade.LedgerVersionNone, false, err
	}
	return entry{key: k, phase: s.Phase, plan: s.Plan, proposed: s.Proposed, identity: s.Identity}, version(ver), true, nil
}

func (f *fileLedger) List(_ context.Context, scope string) ([]facade.LedgerEntry, error) {
	var keys []facade.LedgerKey
	seen := make(map[facade.LedgerKey]struct{})
	err := filepath.WalkDir(f.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), verSuffix) {
			return err
		}
		i := strings.LastIndex(d.Name(), verInfix)
		if i < 0 {
			return nil
		}
		rel, relErr := filepath.Rel(f.root, filepath.Join(filepath.Dir(p), d.Name()[:i]))
		if relErr != nil {
			return nil
		}
		k := facade.LedgerKey(filepath.ToSlash(rel))
		if _, dup := seen[k]; dup || !strings.HasPrefix(string(k), scope) {
			return nil
		}
		seen[k] = struct{}{}
		keys = append(keys, k)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("ledger list %s: %w", scope, err)
	}
	sortKeys(keys)
	out := make([]facade.LedgerEntry, 0, len(keys))
	for _, k := range keys {
		s, _, found, readErr := f.current(k)
		if readErr != nil {
			return nil, readErr
		}
		if !found {
			continue
		}
		out = append(out, entry{key: k, phase: s.Phase, plan: s.Plan, proposed: s.Proposed, identity: s.Identity})
	}
	return out, nil
}

// guard applies the compare-and-swap rules against what is on disk, returning the current state
// and the version the write must follow.
func (f *fileLedger) guard(k facade.LedgerKey, v facade.LedgerVersion) (state, uint64, bool, error) {
	s, ver, found, err := f.current(k)
	if err != nil {
		return state{}, 0, false, err
	}
	switch {
	case v == facade.LedgerVersionNone && found:
		return state{}, 0, false, fmt.Errorf("%w: %s exists", facade.ErrLedgerConflict, k)
	case v == facade.LedgerVersionNone:
		return s, ver, found, nil
	case !found:
		return state{}, 0, false, fmt.Errorf("%w: %s", facade.ErrLedgerNotFound, k)
	case version(ver) != v:
		return state{}, 0, false, fmt.Errorf("%w: %s at %s, presented %s", facade.ErrLedgerConflict, k, version(ver), v)
	}
	return s, ver, found, nil
}

func (f *fileLedger) Begin(_ context.Context, k facade.LedgerKey, proposed []byte, v facade.LedgerVersion) error {
	if cur, _, found, err := f.current(k); err != nil {
		return err
	} else if found && cur.Phase == facade.LedgerPending && bytes.Equal(cur.Proposed, proposed) {
		return nil
	}
	s, ver, _, err := f.guard(k, v)
	if err != nil {
		return err
	}
	// Plan carries through untouched: a call that has not returned may still fail, and n-1 is the
	// one thing that cannot be reconstructed.
	return f.commit(k, state{Phase: facade.LedgerPending, Plan: s.Plan, Proposed: clone(proposed)}, ver)
}

func (f *fileLedger) Resolve(_ context.Context, k facade.LedgerKey, identity []byte, v facade.LedgerVersion) error {
	s, ver, found, err := f.guard(k, v)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: %s", facade.ErrLedgerNotFound, k)
	}
	if s.Phase != facade.LedgerPending {
		return fmt.Errorf("%w: %s is live, expected pending", facade.ErrLedgerPhase, k)
	}
	return f.commit(k, state{Phase: facade.LedgerLive, Plan: s.Proposed, Identity: clone(identity)}, ver)
}

func (f *fileLedger) Forget(_ context.Context, k facade.LedgerKey, v facade.LedgerVersion) error {
	_, ver, found, err := f.guard(k, v)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: %s", facade.ErrLedgerNotFound, k)
	}
	return f.commit(k, state{Tombstone: true}, ver)
}
