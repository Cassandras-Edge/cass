package cmd

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/Cassandras-Edge/cass/internal/auth"
	"github.com/Cassandras-Edge/cass/internal/portal"
)

func ensureKeyCmd() *cobra.Command {
	var quiet, header bool
	cmd := &cobra.Command{
		Use:   "ensure-key <service>",
		Short: "Get or mint a per-service MCP key (the headersHelper hot path)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runEnsureKey(args[0], quiet, header)
		},
	}
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "output only the key (no status messages)")
	cmd.Flags().BoolVarP(&header, "header", "H", false, "output JSON headers for Claude Code headersHelper")
	return cmd
}

func runEnsureKey(service string, quiet, asHeader bool) error {
	// Cache hit: serve the cached key, but re-validate against portal first so
	// stale-cache footguns (key revoked elsewhere) self-heal. Validate returns
	// true on transient errors so we don't thrash on network blips — the MCP
	// server itself is the final gate.
	if cached := auth.GetServiceKey(service); cached != "" {
		c, err := portal.NewClient()
		if err == nil && !c.ValidateKey(cached) {
			_ = auth.ClearServiceKey(service)
			if !quiet && !asHeader {
				fmt.Fprintf(os.Stderr, "Cached key for %s is no longer valid — re-provisioning...\n", service)
			}
		} else {
			writeKey(cached, service, quiet, asHeader, "")
			return nil
		}
	}

	// Cache miss (or just invalidated) — provision a fresh one.
	c, err := portal.NewClient()
	if err != nil {
		return err
	}

	if !quiet && !asHeader {
		fmt.Fprintf(os.Stderr, "Provisioning key for %s...\n", service)
	}

	var projects []struct {
		ID string `json:"id"`
	}
	projectID := "default"
	if err := c.Get("/api/projects", &projects); err == nil && len(projects) > 0 {
		projectID = projects[0].ID
	}

	var created struct {
		Key string `json:"key"`
	}
	body := map[string]string{"name": "cass-cli-" + service}
	path := fmt.Sprintf("/api/projects/%s/services/%s/keys", projectID, service)
	if err := c.Post(path, body, &created); err != nil {
		return fmt.Errorf("create key for %s: %w", service, err)
	}
	if created.Key == "" {
		return fmt.Errorf("portal returned empty key for %s", service)
	}

	if err := auth.SaveServiceKey(service, created.Key, c.Email()); err != nil {
		return fmt.Errorf("cache key: %w", err)
	}

	writeKey(created.Key, service, quiet, asHeader, "created")
	return nil
}

func writeKey(key, service string, quiet, asHeader bool, action string) {
	switch {
	case asHeader:
		out, _ := json.Marshal(map[string]string{"Authorization": "Bearer " + key})
		fmt.Println(string(out))
	case quiet:
		fmt.Println(key)
	default:
		preview := key
		if len(preview) > 20 {
			preview = preview[:20] + "..."
		}
		if action == "created" {
			fmt.Printf("Created key for %s: %s\n", service, preview)
		} else {
			fmt.Printf("Key for %s: %s\n", service, preview)
		}
	}
}
