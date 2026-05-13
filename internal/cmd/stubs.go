package cmd

import "github.com/spf13/cobra"

// addStubs registers placeholder commands for Python subcommands that have
// been intentionally dropped (not deferred). Nothing left right now — every
// non-stubbed command is fully ported.
func addStubs(_ *cobra.Command) {}
