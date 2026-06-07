package cmd

import "github.com/spf13/cobra"

// alias returns a hidden top-level command that delegates verbatim to a
// fresh instance of the real command produced by `make`. It exists purely
// for back-compat: after the v0.8 namespacing reorg, the canonical homes
// for these commands moved under the auth/keys/link/client/tools groups,
// but every historical top-level invocation (`cass whoami`, `cass setup`,
// `cass ensure-key X`, `cass cookies sync`, …) must keep working.
//
// Why a fresh instance instead of re-parenting: a single *cobra.Command can
// live under exactly one parent, and the real command is already attached to
// its namespace group. Constructors are cheap, so the alias builds its own.
//
// DisableFlagParsing makes cobra hand the alias EVERY token after the alias
// name — flags, args, and any subcommand names — untouched. We forward that
// slice into the real command's own Execute(), so its flag set, Args
// validators, and subcommand routing all behave exactly as if the user had
// typed the canonical path. `name` is the alias verb (matches the real
// command's base name so e.g. `cass gmail status` resolves the `status` sub).
func alias(name string, make func() *cobra.Command) *cobra.Command {
	return &cobra.Command{
		Use:                name,
		Hidden:             true,
		DisableFlagParsing: true,
		SilenceUsage:       true,
		RunE: func(_ *cobra.Command, args []string) error {
			real := make()
			real.SetArgs(args)
			// Silence the inner tree's own error/usage printing so the error
			// surfaces exactly once — via the outer root — while the returned
			// error still drives the process exit code.
			real.SilenceErrors = true
			real.SilenceUsage = true
			return real.Execute()
		},
	}
}
