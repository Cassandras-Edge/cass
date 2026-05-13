package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Cassandras-Edge/cass/internal/auth"
)

func whoamiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show current authenticated identity",
		RunE: func(_ *cobra.Command, _ []string) error {
			creds, err := auth.Read()
			if err != nil || creds.MCPKey == "" {
				return fmt.Errorf("not logged in — run: cass login")
			}
			key := creds.MCPKey
			if len(key) > 16 {
				key = key[:16] + "..."
			}
			fmt.Printf("Email:  %s\n", creds.Email)
			fmt.Printf("Key:    %s\n", key)
			fmt.Printf("File:   %s\n", auth.EnvPath())
			return nil
		},
	}
}
