package cmd

import (
	"fmt"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"github.com/Cassandras-Edge/cass/internal/auth"
	"github.com/Cassandras-Edge/cass/internal/portal"
)

func devicesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "devices",
		Short: "List or revoke devices authorized for this account",
	}
	cmd.AddCommand(devicesListCmd())
	cmd.AddCommand(devicesStatusCmd())
	cmd.AddCommand(devicesRevokeCmd())
	return cmd
}

type cliDevice struct {
	ID         string `json:"id"`
	DeviceName string `json:"device_name"`
	CreatedAt  string `json:"created_at"`
}

var (
	tableHeaderStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("63"))
	tableCellStyle   = lipgloss.NewStyle()
)

func devicesListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "Show all devices authorized for this account",
		RunE: func(_ *cobra.Command, _ []string) error {
			c, err := portal.NewClient()
			if err != nil {
				return err
			}
			var resp struct {
				Devices []cliDevice `json:"devices"`
			}
			if err := c.Get("/api/cli/devices", &resp); err != nil {
				return err
			}
			if len(resp.Devices) == 0 {
				fmt.Println("No devices authorized.")
				return nil
			}
			fmt.Println(tableHeaderStyle.Render(fmt.Sprintf("%-22s  %-25s  %s", "ID", "NAME", "CREATED")))
			for _, d := range resp.Devices {
				fmt.Println(tableCellStyle.Render(fmt.Sprintf("%-22s  %-25s  %s", d.ID, d.DeviceName, d.CreatedAt)))
			}
			return nil
		},
	}
}

func devicesStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the currently-loaded credentials on THIS device",
		RunE: func(_ *cobra.Command, _ []string) error {
			creds, err := auth.Read()
			if err != nil || creds.MCPKey == "" {
				return fmt.Errorf("no credentials loaded — run: cass login")
			}
			email := creds.Email
			if email == "" {
				email = "(unknown — re-run cass login to set CASS_EMAIL)"
			}
			key := creds.MCPKey
			if len(key) > 16 {
				key = key[:16] + "..."
			}
			fmt.Printf("Email:    %s\n", email)
			fmt.Printf("CF token: present\n")
			fmt.Printf("MCP key:  %s\n", key)
			fmt.Printf("Env file: %s\n", auth.EnvPath())
			return nil
		},
	}
}

func devicesRevokeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <device-id>",
		Short: "Revoke a device by ID (use `cass devices list` to find IDs)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, err := portal.NewClient()
			if err != nil {
				return err
			}
			code, err := c.Delete("/api/cli/devices/" + args[0])
			if err != nil {
				return err
			}
			if code != 200 {
				return fmt.Errorf("portal returned %d", code)
			}
			fmt.Printf("Revoked %s\n", args[0])
			return nil
		},
	}
}
