package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"

	"github.com/spf13/cobra"

	"github.com/Cassandras-Edge/cass/internal/cassconfig"
)

// configCmd is the surface for managing .cass.toml — the per-directory
// args + env feed for `cass claude` / `cass codex`.
func configCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Manage .cass.toml — per-directory args + env for cass claude/codex",
	}
	cmd.AddCommand(configShowCmd())
	cmd.AddCommand(configEditCmd())
	cmd.AddCommand(configInitCmd())
	return cmd
}

func configShowCmd() *cobra.Command {
	var client string
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Show resolved config + provenance for the current directory",
		RunE: func(_ *cobra.Command, _ []string) error {
			cwd, _ := os.Getwd()
			for _, c := range pickClients(client) {
				resolved, err := cassconfig.LoadForCwd(cwd, c)
				if err != nil {
					return err
				}
				fmt.Printf("── %s ──\n", c)
				if resolved.Source == "" {
					fmt.Println("  (no .cass.toml found — `cass " + c + "` execs with no overrides)")
				} else {
					fmt.Printf("  source: %s\n", resolved.Source)
					if len(resolved.Args) > 0 {
						fmt.Printf("  args:   %v\n", resolved.Args)
					}
					if len(resolved.Env) > 0 {
						fmt.Println("  env:")
						keys := make([]string, 0, len(resolved.Env))
						for k := range resolved.Env {
							keys = append(keys, k)
						}
						sort.Strings(keys)
						for _, k := range keys {
							fmt.Printf("    %s=%s\n", k, resolved.Env[k])
						}
					}
					if len(resolved.Args) == 0 && len(resolved.Env) == 0 {
						fmt.Println("  (section empty)")
					}
				}
				fmt.Println()
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&client, "client", "both", "Which client to show: claude | codex | both")
	return cmd
}

func configEditCmd() *cobra.Command {
	var global bool
	cmd := &cobra.Command{
		Use:   "edit",
		Short: "Open .cass.toml in $EDITOR (creates if missing)",
		Long: `Without --global, edits <cwd>/.cass.toml — creates it from a template
if not present. With --global, edits ~/.cass/config.toml.`,
		RunE: func(_ *cobra.Command, _ []string) error {
			var path string
			if global {
				p, err := cassconfig.HomeGlobalPath()
				if err != nil {
					return err
				}
				path = p
			} else {
				cwd, err := os.Getwd()
				if err != nil {
					return err
				}
				path = filepath.Join(cwd, ".cass.toml")
			}
			if _, err := os.Stat(path); os.IsNotExist(err) {
				if err := cassconfig.WriteTemplate(path); err != nil {
					return err
				}
				fmt.Printf("Created %s\n", path)
			}
			editor := os.Getenv("EDITOR")
			if editor == "" {
				editor = "vim"
			}
			c := exec.Command(editor, path)
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
			return c.Run()
		},
	}
	cmd.Flags().BoolVar(&global, "global", false, "Edit ~/.cass/config.toml instead of <cwd>/.cass.toml")
	return cmd
}

func configInitCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Write a .cass.toml template in the current directory",
		RunE: func(_ *cobra.Command, _ []string) error {
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			path := filepath.Join(cwd, ".cass.toml")
			if err := cassconfig.WriteTemplate(path); err != nil {
				return err
			}
			fmt.Printf("Wrote template to %s\n", path)
			fmt.Println("Edit with `cass config edit` or open in your editor directly.")
			return nil
		},
	}
	return cmd
}

func pickClients(client string) []string {
	switch client {
	case "claude", "codex":
		return []string{client}
	}
	return []string{"claude", "codex"}
}
