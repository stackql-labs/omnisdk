package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
)

// doc-merge exposes the same scoped merge facility used by document queries.
func docMergeCommand() *cobra.Command {
	return &cobra.Command{Use: "doc-merge <registry> <patch-config-json | @file>", Short: "Build an effective registry from document merge patches; print its path", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		raw := []byte(args[1])
		if path, ok := strings.CutPrefix(args[1], "@"); ok {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			raw = data
		}
		var config struct {
			Patches []omnisdk.DocPatch `json:"patches"`
			Cache   omnisdk.DocCache   `json:"doc_cache"`
		}
		if err := json.Unmarshal(raw, &config); err != nil {
			return err
		}
		if len(config.Patches) == 0 {
			return fmt.Errorf("at least one document patch is required")
		}
		registry, err := omnisdk.EffectiveRegistry(args[0], config.Patches, config.Cache)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), registry)
		return err
	}}
}
