package omnisdk

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/stackql-labs/omnisdk/pkg/docparse/stackqldoc"
)

// DocPatch changes one service document, for whoever asks for it and nobody else: a merge patch
// (RFC 7386) applied to the document. It exists because a caller may know something a shared
// document does not — a row path, an operation to poll, a method nobody wrote down — and must be
// able to say so without changing what every other caller reads.
type DocPatch struct {
	// Service is "<provider>.<service>", as addresses name it.
	Service string `json:"service"`
	// Merge is the patch: objects merge key by key, null deletes, anything else replaces.
	Merge json.RawMessage `json:"merge"`
}

// DocCache is where effective registries are kept. Dir is required: which directory a run writes to
// is scope. An entry is reused by every caller whose base and patches are the same; Fresh rebuilds it.
type DocCache struct {
	Dir   string `json:"dir"`
	Fresh bool   `json:"fresh,omitempty"`
}

// NewDocCache makes a new, empty cache directory under parent and returns it: for a caller who wants
// its own location rather than one it has used before.
func NewDocCache(parent string) (string, error) {
	if parent == "" {
		return "", fmt.Errorf("omnisdk: a document cache needs a parent directory")
	}
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", err
	}
	return os.MkdirTemp(parent, "omnisdk-docs-")
}

// EffectiveRegistry returns a registry root holding base with patches applied. Every function taking
// a registry directory accepts it as it accepts base, so reads, mutations and descriptions all see
// the patched documents. With no patches it is base itself.
//
// Only the patched service documents are written; every other file links to base, so an entry costs
// the documents it changes. Entries are keyed by base and patches: the same view is built once and
// shared, and a different one never touches it.
func EffectiveRegistry(base string, patches []DocPatch, cache DocCache) (string, error) {
	if len(patches) == 0 {
		return base, nil
	}
	if cache.Dir == "" {
		return "", fmt.Errorf("omnisdk: patched documents need a cache directory")
	}
	root, err := filepath.Abs(base)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(root, "provider.yaml")); err == nil {
		return "", fmt.Errorf("omnisdk: %s is a single-provider bundle; patches apply to a registry root", base)
	}
	key, err := viewKey(root, patches)
	if err != nil {
		return "", err
	}
	target := filepath.Join(cache.Dir, key)
	if !cache.Fresh {
		if _, err := os.Stat(filepath.Join(target, completeMarker)); err == nil {
			return target, nil
		}
	}
	docs, err := patchedDocs(root, patches)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(cache.Dir, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(cache.Dir, ".build-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp) // a no-op once renamed into place
	if err := mirror(root, tmp, docs); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(tmp, completeMarker), nil, 0o644); err != nil {
		return "", err
	}
	return target, publish(tmp, target, cache.Fresh)
}

// completeMarker is written last: an entry without it is a build that did not finish.
const completeMarker = ".complete"

// viewKey names a view by its base and its patches, each patch reduced to canonical JSON so that
// formatting does not split one view into two.
func viewKey(root string, patches []DocPatch) (string, error) {
	h := sha256.New()
	h.Write([]byte(root))
	for _, p := range patches {
		var v any
		if err := json.Unmarshal(p.Merge, &v); err != nil {
			return "", fmt.Errorf("omnisdk: patch for %s is not JSON: %w", p.Service, err)
		}
		b, _ := json.Marshal(v)
		h.Write([]byte{0})
		h.Write([]byte(p.Service))
		h.Write([]byte{0})
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))[:24], nil
}

// patchedDocs reads each patched service document, applies its patches in order, and returns the
// results by path relative to the registry root.
func patchedDocs(root string, patches []DocPatch) (map[string][]byte, error) {
	reg, err := stackqldoc.OpenRegistry(os.DirFS(root))
	if err != nil {
		return nil, err
	}
	locator, ok := reg.(interface{ ServiceFile(string) (string, error) })
	if !ok {
		return nil, fmt.Errorf("omnisdk: this registry cannot locate service documents")
	}
	docs := map[string]any{}
	var order []string
	for _, p := range patches {
		rel, err := locator.ServiceFile(p.Service)
		if err != nil {
			return nil, err
		}
		doc, seen := docs[rel]
		if !seen {
			b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
			if err != nil {
				return nil, err
			}
			if err := yaml.Unmarshal(b, &doc); err != nil {
				return nil, fmt.Errorf("omnisdk: %s: %w", rel, err)
			}
			order = append(order, rel)
		}
		var patch any
		if err := json.Unmarshal(p.Merge, &patch); err != nil {
			return nil, fmt.Errorf("omnisdk: patch for %s is not JSON: %w", p.Service, err)
		}
		docs[rel] = mergePatch(doc, patch)
	}
	out := make(map[string][]byte, len(docs))
	for _, rel := range order {
		b, err := yaml.Marshal(docs[rel])
		if err != nil {
			return nil, fmt.Errorf("omnisdk: %s: %w", rel, err)
		}
		out[rel] = b
	}
	return out, nil
}

// mergePatch is RFC 7386: an object patch merges into an object key by key, a null removes the key,
// and anything else replaces the target outright — arrays included.
func mergePatch(target, patch any) any {
	p, ok := patch.(map[string]any)
	if !ok {
		return patch
	}
	t, ok := target.(map[string]any)
	if !ok {
		t = map[string]any{}
	}
	out := make(map[string]any, len(t)+len(p))
	for k, v := range t {
		out[k] = v
	}
	for k, v := range p {
		if v == nil {
			delete(out, k)
			continue
		}
		out[k] = mergePatch(out[k], v)
	}
	return out
}

// mirror recreates base's directories under out, writes the patched documents, and links every other
// file back to base. Directories are real so that a registry scan sees them as directories.
func mirror(root, out string, docs map[string][]byte) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil || rel == "." {
			return err
		}
		dst := filepath.Join(out, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(dst, 0o755)
		case docs[filepath.ToSlash(rel)] != nil:
			return os.WriteFile(dst, docs[filepath.ToSlash(rel)], 0o644)
		}
		return os.Symlink(p, dst)
	})
}

// publish moves a finished build into place. A concurrent build of the same view that got there
// first wins, which is harmless: both built the same thing. Fresh replaces whatever is there.
func publish(tmp, target string, fresh bool) error {
	if fresh {
		if _, err := os.Stat(target); err == nil {
			old := target + ".old-" + filepath.Base(tmp)
			if err := os.Rename(target, old); err != nil {
				return err
			}
			defer os.RemoveAll(old)
		}
	}
	if err := os.Rename(tmp, target); err != nil {
		if _, statErr := os.Stat(filepath.Join(target, completeMarker)); statErr == nil && !fresh {
			return nil
		}
		return errors.Join(fmt.Errorf("omnisdk: publish document view"), err)
	}
	return nil
}
