package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Cassandras-Edge/cass/internal/auth"
)

func logoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Remove cached credentials (does not revoke the device on the portal)",
		RunE: func(_ *cobra.Command, _ []string) error {
			if err := auth.Remove(); err != nil {
				return err
			}
			fmt.Printf("Removed %s\n", auth.EnvPath())
			return nil
		},
	}
}
