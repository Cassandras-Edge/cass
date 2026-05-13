package cmd

import (
	"github.com/spf13/cobra"

	"github.com/Cassandras-Edge/cass/internal/tui"
)

// Version is stamped at build time via:
//
//	go build -ldflags="-X github.com/Cassandras-Edge/cass/internal/cmd.Version=v0.7.4"
//
// `scripts/build.sh` does this automatically from `git describe --tags`.
var Version = "dev"

func New() *cobra.Command {
	root := &cobra.Command{
		Use:     "cass",
		Short:   "Cassandra platform CLI",
		Long:    "Auth, keys, cookies, and service management for the Cassandra platform.",
		Version: Version,
		// No subcommand → drop into the interactive dashboard.
		RunE: func(_ *cobra.Command, _ []string) error {
			return tui.Run()
		},
		SilenceUsage: true,
	}
	root.AddCommand(loginCmd())
	root.AddCommand(logoutCmd())
	root.AddCommand(whoamiCmd())
	root.AddCommand(ensureKeyCmd())
	root.AddCommand(devicesCmd())
	root.AddCommand(keysCmd())
	root.AddCommand(refreshKeysCmd())
	root.AddCommand(updateCmd())
	root.AddCommand(installCmd())
	root.AddCommand(shareCmd())
	root.AddCommand(imageCmd())
	root.AddCommand(patchedCLICmd())
	root.AddCommand(twitterCmd())
	root.AddCommand(cookiesCmd())
	root.AddCommand(setupCmd())
	root.AddCommand(teardownCmd())
	root.AddCommand(claudeCmd())
	root.AddCommand(codexCmd())
	root.AddCommand(configCmd())
	root.AddCommand(authGroupCmd())
	root.AddCommand(discordCmd())
	root.AddCommand(gmailCmd())
	addStubs(root)
	return root
}
