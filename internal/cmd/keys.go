package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Cassandras-Edge/cass/internal/auth"
)

func keysCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "keys",
		Short: "Inspect cached service keys (under ~/.cass/keys/)",
	}
	cmd.AddCommand(keysListCmd())
	cmd.AddCommand(keysClearCmd())
	return cmd
}

func keysListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List service keys cached on this device",
		RunE: func(_ *cobra.Command, _ []string) error {
			dir := auth.KeysDir()
			entries, err := os.ReadDir(dir)
			if err != nil {
				if os.IsNotExist(err) {
					fmt.Println("No cached keys.")
					return nil
				}
				return err
			}
			var services []string
			for _, e := range entries {
				if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
					services = append(services, strings.TrimSuffix(e.Name(), ".json"))
				}
			}
			if len(services) == 0 {
				fmt.Println("No cached keys.")
				return nil
			}
			sort.Strings(services)
			fmt.Println(tableHeaderStyle.Render(fmt.Sprintf("%-22s  %s", "SERVICE", "KEY")))
			for _, s := range services {
				k := auth.GetServiceKey(s)
				if len(k) > 22 {
					k = k[:22] + "..."
				}
				fmt.Printf("%-22s  %s\n", s, k)
			}
			return nil
		},
	}
}

func keysClearCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "clear [service]",
		Short: "Remove a cached service key (or --all to wipe the cache)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if all {
				dir := auth.KeysDir()
				entries, err := os.ReadDir(dir)
				if err != nil {
					if os.IsNotExist(err) {
						fmt.Println("Cache already empty.")
						return nil
					}
					return err
				}
				n := 0
				for _, e := range entries {
					if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
						if err := os.Remove(filepath.Join(dir, e.Name())); err == nil {
							n++
						}
					}
				}
				fmt.Printf("Cleared %d cached key(s).\n", n)
				return nil
			}
			if len(args) == 0 {
				return fmt.Errorf("specify a service name or use --all")
			}
			if err := auth.ClearServiceKey(args[0]); err != nil {
				return err
			}
			fmt.Printf("Cleared cache for %s.\n", args[0])
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "Clear all cached service keys")
	return cmd
}
