package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Cassandras-Edge/cass/internal/auth"
	"github.com/Cassandras-Edge/cass/internal/claudecfg"
	"github.com/Cassandras-Edge/cass/internal/manifest"
	"github.com/Cassandras-Edge/cass/internal/portal"
	"github.com/Cassandras-Edge/cass/internal/registry"
)

// nearExpiryDays — re-mint plugin keys with less than this many days left.
// Matches the same 7-day window setup uses. SessionStart hooks that call
// `cass refresh-keys --if-near-expiry` become a fast no-op when all keys
// are healthy.
const nearExpiryDays = 7

func refreshKeysCmd() *cobra.Command {
	var (
		force          bool
		serviceFilter  string
		ifNearExpiry   bool
	)
	cmd := &cobra.Command{
		Use:   "refresh-keys",
		Short: "Rotate MCP bearer tokens in ~/.claude/settings.json (and Codex config)",
		Long: `Rotates per-service MCP keys for every Cassandra service registered
in the user-scope settings.json. Reuses cached keys when healthy;
re-mints when revoked or near expiry.

With --if-near-expiry, only rotates keys whose expiry is within 7 days.
Designed for a Claude Code SessionStart hook — fast no-op when keys are
healthy, self-heals when they're about to expire.

Note: this rotates tokens only. Use 'cass setup' to also refresh
manifests + skills.`,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runRefreshKeys(force, serviceFilter, ifNearExpiry)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "Re-mint every key, ignoring the local cache")
	cmd.Flags().StringVar(&serviceFilter, "service", "", "Rotate only this service")
	cmd.Flags().BoolVar(&ifNearExpiry, "if-near-expiry", false,
		"Only rotate keys whose expiry is within 7 days (SessionStart-hook friendly)")
	return cmd
}

func runRefreshKeys(force bool, serviceFilter string, ifNearExpiry bool) error {
	if serviceFilter != "" && registry.Find(serviceFilter) == nil {
		known := make([]string, 0, len(registry.Services))
		for _, s := range registry.Services {
			known = append(known, s.Name)
		}
		sort.Strings(known)
		return fmt.Errorf("unknown service %q. Known: %s", serviceFilter, strings.Join(known, ", "))
	}

	client, err := portal.NewClient()
	if err != nil {
		return err
	}
	projectID, _ := defaultProjectID(client)

	settings, err := claudecfg.LoadSettings(claudecfg.ScopeUser)
	if err != nil {
		return err
	}

	type result struct {
		service string
		source  string
		err     error
	}
	var results []result
	rotated := 0

	for _, svc := range registry.Services {
		if serviceFilter != "" && svc.Name != serviceFilter {
			continue
		}
		// Only rotate for services already registered in settings.json
		// (don't accidentally install a service the user didn't opt in to).
		if !claudecfg.HasMCPServer(settings, svc.Name) {
			continue
		}

		var key, source string
		if !force {
			if cached := auth.GetServiceKey(svc.Name); cached != "" {
				key, source = cached, "cached"
				if ifNearExpiry {
					reason, refresh := keyNeedsRefresh(client, cached)
					if refresh {
						key, source = "", ""
						fmt.Printf("Re-minting %s key (%s)...\n", svc.Name, reason)
					}
				}
			}
		}
		if key == "" {
			fmt.Printf("Creating key for %s...\n", svc.Name)
			k, err := mintKey(client, projectID, svc.Name)
			if err != nil {
				results = append(results, result{service: svc.Name, err: err})
				continue
			}
			if err := auth.SaveServiceKey(svc.Name, k, client.Email()); err != nil {
				results = append(results, result{service: svc.Name, err: err})
				continue
			}
			key, source = k, "new"
		}

		// Refresh the Authorization header in place — preserve existing URL.
		bundle, _ := manifest.Fetch(svc.Repo, svc.Name, false)
		url := ""
		if bundle != nil && bundle.Manifest.HasMCP() {
			url = bundle.Manifest.MCP.URL
		} else {
			// Fall back to URL from settings if manifest fetch failed
			url = existingMCPURL(settings, svc.Name)
		}
		if url == "" {
			results = append(results, result{
				service: svc.Name,
				err:     errors.New("no URL available — re-run `cass setup` to register"),
			})
			continue
		}
		claudecfg.UpsertMCPServer(settings, svc.Name, claudecfg.MCPServerSpec{
			Type: "http",
			URL:  url,
			Headers: map[string]string{
				"Authorization": "Bearer " + key,
			},
		})
		rotated++
		results = append(results, result{service: svc.Name, source: source})
	}

	if err := claudecfg.SaveSettings(claudecfg.ScopeUser, settings); err != nil {
		return fmt.Errorf("write settings: %w", err)
	}

	fmt.Println()
	fmt.Printf("Rotated %d key(s) in %s:\n", rotated, mustSettingsPath())
	for _, r := range results {
		if r.err == nil {
			fmt.Printf("  - %-20s [%s]\n", r.service, r.source)
		}
	}
	failed := 0
	for _, r := range results {
		if r.err != nil {
			failed++
			fmt.Fprintf(os.Stderr, "  ! %-20s %v\n", r.service, r.err)
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d service(s) failed", failed)
	}
	return nil
}

// existingMCPURL reads the URL of a registered MCP server from settings.
// Used as a fallback when manifest fetch fails.
func existingMCPURL(settings map[string]any, name string) string {
	servers, _ := settings["mcpServers"].(map[string]any)
	if servers == nil {
		return ""
	}
	entry, _ := servers[name].(map[string]any)
	if entry == nil {
		return ""
	}
	url, _ := entry["url"].(string)
	return url
}

func mustSettingsPath() string {
	p, _ := claudecfg.SettingsPath(claudecfg.ScopeUser)
	return p
}

// keyNeedsRefresh checks if a cached service key should be rotated. Returns
// (reason, true) when re-mint is needed; (_, false) when the key is healthy or
// when validation hit a transient error (we don't thrash on network blips).
func keyNeedsRefresh(c *portal.Client, key string) (string, bool) {
	w, err := c.WhoamiWithKey(key)
	if err != nil {
		if errors.Is(err, portal.ErrKeyNotValid) {
			return "revoked/expired", true
		}
		return "", false // network error — let MCP server be the final gate
	}
	if w.ExpiresAt == "" {
		return "", false
	}
	t, err := time.Parse(time.RFC3339, w.ExpiresAt)
	if err != nil {
		t, err = time.Parse("2006-01-02T15:04:05.000Z", w.ExpiresAt)
		if err != nil {
			return "", false
		}
	}
	if time.Until(t) <= time.Duration(nearExpiryDays)*24*time.Hour {
		return fmt.Sprintf("expires in %s", time.Until(t).Round(time.Hour)), true
	}
	return "", false
}

func mintKey(c *portal.Client, projectID, service string) (string, error) {
	var resp struct {
		Key string `json:"key"`
	}
	body := map[string]string{"name": "cass-cli-" + service}
	path := fmt.Sprintf("/api/projects/%s/services/%s/keys", projectID, service)
	if err := c.Post(path, body, &resp); err != nil {
		return "", err
	}
	if resp.Key == "" {
		return "", fmt.Errorf("portal returned empty key")
	}
	return resp.Key, nil
}

// keep unused-import noise away during incremental edits
var _ = json.Marshal
var _ = filepath.Separator
