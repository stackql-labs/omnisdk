package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDocMergeCommandUsesScopedCache(t *testing.T) {
	base := filepath.Join("..", "..", "pkg", "omnisdk", "testdata", "authreg")
	relative := filepath.Join("widgets", "v00.00.00000", "services", "things.yaml")
	before, err := os.ReadFile(filepath.Join(base, relative))
	if err != nil {
		t.Fatal(err)
	}
	config := map[string]any{"patches": []any{map[string]any{"service": "stackql_unstable_widgets.things", "merge": map[string]any{"info": map[string]string{"description": "generic merge command evidence"}}}}, "doc_cache": map[string]string{"dir": t.TempDir()}}
	raw, _ := json.Marshal(config)
	cmd := docMergeCommand()
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetArgs([]string{base, string(raw)})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	merged, err := os.ReadFile(filepath.Join(strings.TrimSpace(output.String()), relative))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(merged, []byte("generic merge command evidence")) {
		t.Fatal("patch missing from effective registry")
	}
	after, err := os.ReadFile(filepath.Join(base, relative))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("source registry was changed")
	}
}
