package cmd

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Cassandras-Edge/cass/internal/claudecfg"
	"github.com/Cassandras-Edge/cass/internal/registry"
)

// addCmd installs individual Cassandra MCP service(s) without the full
// `cass setup` ceremony — no client/scope form, no service picker, no
// hooks or shell rebind. It reuses the same sync paths as setup but with
// an allow-list of (already installed ∪ requested) so nothing else gets
// pruned or touched.
func addCmd() *cobra.Command {
	var client, scope string
	var force bool
	cmd := &cobra.Command{
		Use:   "add <service> [service...]",
		Short: "Add individual MCP service(s) to Claude/Codex directly",
		Long: `Registers one or more Cassandra MCP services with the chosen client(s),
minting per-service keys as needed. Existing services are left untouched.

Run with no args to list available service names.`,
		RunE: func(_ *cobra.Command, args []string) error {
			if err := validateChoice(client, []string{"auto", "claude", "codex", "both"}, "--client"); err != nil {
				return err
			}
			if err := validateChoice(scope, scopeChoices, "--scope"); err != nil {
				return err
			}
			if len(args) == 0 {
				printAddCatalog()
				return nil
			}
			return runAdd(args, client, scope, force)
		},
	}
	cmd.Flags().StringVar(&client, "client", "auto", "Which client to add to: auto | claude | codex | both")
	cmd.Flags().StringVar(&scope, "scope", "user", "Settings scope: user | project | local")
	cmd.Flags().BoolVar(&force, "force", false, "Ignore the manifest cache and re-fetch")
	return cmd
}

func runAdd(names []string, client, scope string, force bool) error {
	clients, err := resolveClients(client)
	if err != nil {
		return err
	}

	// Validate every requested name up front against the union of both
	// catalogs so a typo fails the whole run instead of half-applying.
	for _, n := range names {
		_, codexKnown := codexServers[n]
		if registry.Find(n) == nil && !codexKnown {
			return fmt.Errorf("unknown service %q — run `cass add` with no args to list available services", n)
		}
	}

	if contains(clients, "claude") {
		if err := ensureGhAuthed(); err != nil {
			return err
		}
		var claudeNames []string
		for _, n := range names {
			if registry.Find(n) != nil {
				claudeNames = append(claudeNames, n)
			}
		}
		if len(claudeNames) > 0 {
			allow := union(installedClaudeRegistryServices(scope), claudeNames)
			if err := syncClaudeDirect(scope, nil, force, false, allow, false); err != nil {
				return err
			}
			fmt.Println("\nRestart Claude Code to pick up the new MCP server(s).")
		}
	}

	if contains(clients, "codex") {
		var codexNames []string
		for _, n := range names {
			if _, ok := codexServers[n]; ok {
				codexNames = append(codexNames, n)
			}
		}
		if len(codexNames) > 0 {
			fmt.Println()
			installed := []string{}
			for n := range installedCodexServers(codexScopeFor(scope)) {
				if _, ok := codexServers[n]; ok {
					installed = append(installed, n)
				}
			}
			syncCodex(true, nil, codexScopeFor(scope), union(installed, codexNames))
			fmt.Println("\nRestart Codex to activate the MCP server(s).")
		}
	}
	return nil
}

// installedClaudeRegistryServices returns the Cassandra registry services
// already present in ~/.claude.json at the given scope. Used to build the
// allow-list so `cass add` is purely additive.
func installedClaudeRegistryServices(scope string) []string {
	store, err := claudecfg.LoadMCPStore()
	if err != nil {
		return nil
	}
	present, err := store.ListMCPs(mapClaudeScope(scope))
	if err != nil {
		return nil
	}
	out := []string{}
	for _, n := range present {
		if registry.Find(n) != nil {
			out = append(out, n)
		}
	}
	return out
}

func printAddCatalog() {
	names := map[string]string{}
	for _, s := range registry.Services {
		names[s.Name] = s.Description
	}
	for n, spec := range codexServers {
		if _, ok := names[n]; !ok {
			names[n] = spec.Description
		}
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	width := 0
	for _, n := range sorted {
		if len(n) > width {
			width = len(n)
		}
	}
	fmt.Println("Available services:")
	for _, n := range sorted {
		fmt.Printf("  %s  %s\n", n+strings.Repeat(" ", width-len(n)), names[n])
	}
	fmt.Println("\nUsage: cass add <service> [--client claude|codex|both] [--scope user|project]")
}
