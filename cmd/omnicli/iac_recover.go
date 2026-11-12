package main

import (
	"github.com/spf13/cobra"
)

func iacRecoverCommand(run func(*cobra.Command, []string) error) *cobra.Command {
	cmd := &cobra.Command{Use: "iac-recover", Short: "Record an operator-verified outcome for one pending resource; no cloud calls", RunE: run}

	cmd.Flags().String("name", "", "collection (required)")
	cmd.Flags().String("state", "", "existing state directory (required)")
	cmd.Flags().String("key", "", "pending resource key (required)")
	cmd.Flags().String("outcome", "", "operator-verified absent or applied outcome (required)")
	cmd.Flags().String("identity", "", "verified resource identity; required only for applied")
	cmd.Flags().String("version", "", "observed pending ledger version (required)")
	cmd.Flags().String("run-id", "", "fresh recovery journal identifier")
	for _, name := range []string{"name", "state", "key", "outcome", "version"} {
		_ = cmd.MarkFlagRequired(name)
	}
	return cmd
}
