package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Cassandras-Edge/cass/internal/auth"
	"github.com/Cassandras-Edge/cass/internal/portal"
)

const (
	marketplaceName = "cassandra-plugins"
)

// pluginServices maps the Claude Code plugin name to its cass service name.
// Most match 1:1; a few legacy mismatches (media-mcp → yt-mcp, gateway-mcp → gateway,
// routines-mcp → routines).
var pluginServices = map[string]string{
	"tradingview-mcp": "tradingview-mcp",
	"twitter-mcp":     "twitter-mcp",
	"reddit-mcp":      "reddit-mcp",
	"claudeai-mcp":    "claudeai-mcp",
	"discord-mcp":     "discord-mcp",
	"media-mcp":       "yt-mcp",
	"market-research": "market-research",
	"gemini-mcp":      "gemini-mcp",
	"perplexity-mcp":  "perplexity-mcp",
	"gateway-mcp":     "gateway",
	"routines-mcp":    "routines",
	"schwab-mcp":      "schwab-mcp",
}

func envVarFor(plugin string) string {
	return "MCP_KEY_" + strings.ToUpper(strings.ReplaceAll(plugin, "-", "_"))
}

func settingsPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "settings.json")
}

func keysEnvPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cass", "keys.env")
}

// nearExpiryDays — re-mint plugin keys with less than this many days left.
// Matches the same 7-day window setup uses. SessionStart hooks that call
// `cass refresh-keys --if-near-expiry` become a fast no-op when all keys
// are healthy.
const nearExpiryDays = 7

func refreshKeysCmd() *cobra.Command {
	var (
		force        bool
		pluginFilter string
		writeEnv     bool
		launchctlSet bool
		ifNearExpiry bool
	)
	cmd := &cobra.Command{
		Use:   "refresh-keys",
		Short: "Provision MCP keys for every plugin and write them into ~/.claude/settings.json",
		Long: `Fetches a bearer token per Cassandra plugin, caches each under
~/.cass/keys/, and writes them to ~/.claude/settings.json under
pluginConfigs[<plugin>@cassandra-plugins].options.mcpKey.

Plugins that reference ${user_config.mcpKey} in their manifest pick the
token up at MCP load time, avoiding a per-connection cass invocation.

With --if-near-expiry, only rotates keys whose expiry is within 7 days.
Designed for a Claude Code SessionStart hook — fast no-op when keys are
healthy, self-heals when they're about to expire.`,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runRefreshKeys(force, pluginFilter, writeEnv, launchctlSet, ifNearExpiry)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "Re-mint every key, ignoring the local cache")
	cmd.Flags().StringVar(&pluginFilter, "plugin", "", "Refresh only this plugin")
	cmd.Flags().BoolVar(&writeEnv, "write-env", true, "Also write ~/.cass/keys.env (sourcable exports)")
	cmd.Flags().BoolVar(&launchctlSet, "launchctl-setenv", false,
		"After writing keys.env, push each MCP_KEY_* into launchd so GUI-launched Claude Code sees them")
	cmd.Flags().BoolVar(&ifNearExpiry, "if-near-expiry", false,
		"Only rotate keys whose expiry is within 7 days (SessionStart-hook friendly)")
	return cmd
}

func runRefreshKeys(force bool, pluginFilter string, writeEnv, launchctlSet, ifNearExpiry bool) error {
	if pluginFilter != "" {
		if _, ok := pluginServices[pluginFilter]; !ok {
			known := make([]string, 0, len(pluginServices))
			for k := range pluginServices {
				known = append(known, k)
			}
			sort.Strings(known)
			return fmt.Errorf("unknown plugin %q. Known: %s", pluginFilter, strings.Join(known, ", "))
		}
	}

	client, err := portal.NewClient()
	if err != nil {
		return err
	}

	// Resolve the user's default project once; reused for every minted key.
	var projects []struct {
		ID string `json:"id"`
	}
	projectID := "default"
	if err := client.Get("/api/projects", &projects); err == nil && len(projects) > 0 {
		projectID = projects[0].ID
	}

	settings, err := loadSettings()
	if err != nil {
		return err
	}

	type result struct {
		plugin string
		source string
		err    error
	}
	var results []result
	pluginKeys := map[string]string{}

	for plugin, service := range pluginServices {
		if pluginFilter != "" && plugin != pluginFilter {
			continue
		}
		var key, source string
		if !force {
			if cached := auth.GetServiceKey(service); cached != "" {
				key, source = cached, "cached"

				// --if-near-expiry: validate the cached key's expires_at; only
				// re-mint when it's revoked or within 7 days of expiry.
				if ifNearExpiry {
					reason, refresh := keyNeedsRefresh(client, cached)
					if refresh {
						key, source = "", "" // fall through to re-mint
						fmt.Printf("Re-minting %s key (%s)...\n", plugin, reason)
					}
				}
			}
		}
		if key == "" {
			fmt.Printf("Creating key for %s...\n", service)
			k, err := mintKey(client, projectID, service)
			if err != nil {
				results = append(results, result{plugin: plugin, err: err})
				continue
			}
			if err := auth.SaveServiceKey(service, k, client.Email()); err != nil {
				results = append(results, result{plugin: plugin, err: err})
				continue
			}
			key, source = k, "new"
		}
		writePluginOption(settings, plugin, "mcpKey", key)
		pluginKeys[plugin] = key
		results = append(results, result{plugin: plugin, source: source})
	}

	if err := saveSettings(settings); err != nil {
		return fmt.Errorf("write settings: %w", err)
	}

	if writeEnv && len(pluginKeys) > 0 {
		if err := writeKeysEnv(pluginKeys); err != nil {
			return err
		}
		if launchctlSet {
			pushLaunchctl(pluginKeys)
		}
	}

	updated := 0
	failed := 0
	for _, r := range results {
		if r.err != nil {
			failed++
		} else {
			updated++
		}
	}

	fmt.Println()
	fmt.Printf("Wrote %d key(s) to %s:\n", updated, settingsPath())
	for _, r := range results {
		if r.err == nil {
			fmt.Printf("  - %-20s [%s]\n", r.plugin, r.source)
		}
	}
	if writeEnv && len(pluginKeys) > 0 {
		fmt.Printf("\nAlso wrote env file to %s:\n", keysEnvPath())
		fmt.Println("  Source it in your shell profile to use ${env.MCP_KEY_*} in plugin manifests:")
		fmt.Printf("    echo 'source %s' >> ~/.zprofile\n", keysEnvPath())
		if !launchctlSet {
			fmt.Println("  For GUI-launched Claude Code, also run:")
			fmt.Println("    cass refresh-keys --launchctl-setenv")
		}
	}
	if failed > 0 {
		fmt.Fprintf(os.Stderr, "\nFailed for %d plugin(s):\n", failed)
		for _, r := range results {
			if r.err != nil {
				fmt.Fprintf(os.Stderr, "  - %-20s %v\n", r.plugin, r.err)
			}
		}
	}
	fmt.Println()
	fmt.Println("Restart Claude Code for plugins to pick up the new config.")
	return nil
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

func loadSettings() (map[string]any, error) {
	data, err := os.ReadFile(settingsPath())
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("~/.claude/settings.json malformed: %w", err)
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

func saveSettings(settings map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(settingsPath()), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(settingsPath(), append(data, '\n'), 0o644)
}

func writePluginOption(settings map[string]any, plugin, key, value string) {
	pluginID := plugin + "@" + marketplaceName
	configs, _ := settings["pluginConfigs"].(map[string]any)
	if configs == nil {
		configs = map[string]any{}
		settings["pluginConfigs"] = configs
	}
	entry, _ := configs[pluginID].(map[string]any)
	if entry == nil {
		entry = map[string]any{}
		configs[pluginID] = entry
	}
	options, _ := entry["options"].(map[string]any)
	if options == nil {
		options = map[string]any{}
		entry["options"] = options
	}
	options[key] = value
}

func writeKeysEnv(pluginKeys map[string]string) error {
	p := keysEnvPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("# Generated by `cass refresh-keys` — do not edit by hand.\n")
	b.WriteString("# Source this in ~/.zprofile (or .zshrc) to expose plugin keys as env vars.\n\n")
	names := make([]string, 0, len(pluginKeys))
	for k := range pluginKeys {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, plugin := range names {
		fmt.Fprintf(&b, "export %s='%s'\n", envVarFor(plugin), pluginKeys[plugin])
	}
	return os.WriteFile(p, []byte(b.String()), 0o600)
}

func pushLaunchctl(pluginKeys map[string]string) {
	for plugin, key := range pluginKeys {
		_ = exec.Command("launchctl", "setenv", envVarFor(plugin), key).Run()
	}
}
