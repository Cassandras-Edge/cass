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

	"github.com/Cassandras-Edge/cass/internal/auth"
	"github.com/Cassandras-Edge/cass/internal/claudecfg"
	"github.com/Cassandras-Edge/cass/internal/manifest"
	"github.com/Cassandras-Edge/cass/internal/portal"
	"github.com/Cassandras-Edge/cass/internal/registry"
)

// syncClaudeDirect is the new direct-write replacement for the old
// plugin-marketplace ceremony. For each opted-in service: fetch its
// manifest from GitHub, mint a per-service MCP key, write the mcpServers
// entry to settings.json, drop SKILL.md, and install any required
// SessionStart hooks. Then auto-uninstall any legacy
// `<plugin>@cassandra-plugins` entries.
func syncClaudeDirect(scope string, optIn []string, force bool, quiet bool) error {
	cfgScope := mapClaudeScope(scope)

	settings, err := claudecfg.LoadSettings(cfgScope)
	if err != nil {
		return err
	}

	logf := func(format string, args ...any) {
		if !quiet {
			fmt.Printf(format, args...)
		}
	}

	// 1. Migrate away from the legacy plugin marketplace if any traces
	//    remain in the chosen scope's settings.json.
	migrated := pruneLegacyPlugins(settings)
	if migrated > 0 {
		logf("Pruned %d legacy cassandra-plugins entries from %s settings.\n", migrated, scope)
	}

	// 2. Resolve target services (defaults + opt-ins).
	targets := resolveTargetServices(optIn)
	if len(targets) == 0 {
		logf("No Cassandra services selected.\n")
		return claudecfg.SaveSettings(cfgScope, settings)
	}

	// 3. Portal client (needed for key minting). Resolve user's default
	//    project once so all keys land in the same bucket.
	client, perr := portal.NewClient()
	if perr != nil {
		return fmt.Errorf("portal: %w (run `cass login`)", perr)
	}
	projectID, err := defaultProjectID(client)
	if err != nil {
		return err
	}

	// 4. Per-service: fetch manifest, mint key, register.
	var registered, skipped []string
	for _, svc := range targets {
		logf("\n── %s ──\n", svc.Name)
		bundle, err := manifest.Fetch(svc.Repo, svc.Name, force)
		if err != nil {
			// Stale-cache fallback already returns a bundle with an error
			// describing the staleness; treat that as a non-fatal warning.
			if bundle != nil {
				logf("  warning: %v\n", err)
			} else {
				logf("  skipping (no manifest available yet): %v\n", err)
				skipped = append(skipped, svc.Name)
				continue
			}
		}
		m := bundle.Manifest

		if m.HasMCP() {
			key, err := getOrMintKey(client, projectID, svc.Name)
			if err != nil {
				logf("  warning: mint key: %v\n", err)
				skipped = append(skipped, svc.Name)
				continue
			}
			claudecfg.UpsertMCPServer(settings, svc.Name, claudecfg.MCPServerSpec{
				Type: "http",
				URL:  m.MCP.URL,
				Headers: map[string]string{
					"Authorization": "Bearer " + key,
				},
			})
			logf("  mcp:    %s\n", m.MCP.URL)
		}

		if bundle.Skill != "" {
			changed, err := claudecfg.WriteSkill(svc.Name, bundle.Skill)
			if err != nil {
				logf("  warning: write skill: %v\n", err)
			} else if changed {
				logf("  skill:  ~/.claude/skills/%s/SKILL.md (updated)\n", svc.Name)
			} else {
				logf("  skill:  ~/.claude/skills/%s/SKILL.md (unchanged)\n", svc.Name)
			}
		}

		if m.CookieSync != "" {
			if claudecfg.EnsureCookieSyncHook(settings, m.CookieSync) {
				logf("  hook:   cass cookies sync %s (added)\n", m.CookieSync)
			}
		}

		registered = append(registered, svc.Name)
	}

	// 5. Auto-update SessionStart hook.
	if claudecfg.EnsureAutoUpdateHook(settings) {
		logf("\n  hook: cass update on session start (added)\n")
	}

	if err := claudecfg.SaveSettings(cfgScope, settings); err != nil {
		return err
	}

	logf("\nRegistered %d service(s)", len(registered))
	if len(registered) > 0 {
		logf(": %s", strings.Join(registered, ", "))
	}
	logf(".\n")
	if len(skipped) > 0 {
		logf("Skipped %d (no manifest yet or fetch failed): %s\n",
			len(skipped), strings.Join(skipped, ", "))
	}
	return nil
}

// resolveTargetServices returns the services to install. Always includes
// registry defaults; adds optionals only when explicitly opted in.
//
// Special value "all" in optIn includes every optional service.
func resolveTargetServices(optIn []string) []registry.Service {
	wantOpt := map[string]bool{}
	for _, name := range optIn {
		for _, piece := range strings.Split(name, ",") {
			p := strings.TrimSpace(piece)
			if p == "" {
				continue
			}
			wantOpt[p] = true
		}
	}
	all := wantOpt["all"]

	out := []registry.Service{}
	for _, s := range registry.Services {
		if !s.Optional {
			out = append(out, s)
			continue
		}
		if all || wantOpt[s.Name] {
			out = append(out, s)
		}
	}
	return out
}

// pruneLegacyPlugins removes any cassandra-plugins traces from settings.
// Touches enabledPlugins, pluginConfigs, and extraKnownMarketplaces.
// Returns the number of plugin keys removed (marketplace counts as one).
func pruneLegacyPlugins(settings map[string]any) int {
	removed := 0

	if enabled, ok := settings["enabledPlugins"].(map[string]any); ok {
		for k := range enabled {
			if strings.HasSuffix(k, "@cassandra-plugins") {
				delete(enabled, k)
				removed++
			}
		}
		if len(enabled) == 0 {
			delete(settings, "enabledPlugins")
		}
	}
	if configs, ok := settings["pluginConfigs"].(map[string]any); ok {
		for k := range configs {
			if strings.HasSuffix(k, "@cassandra-plugins") {
				delete(configs, k)
			}
		}
		if len(configs) == 0 {
			delete(settings, "pluginConfigs")
		}
	}
	if markets, ok := settings["extraKnownMarketplaces"].(map[string]any); ok {
		if _, has := markets["cassandra-plugins"]; has {
			delete(markets, "cassandra-plugins")
			removed++
		}
		if len(markets) == 0 {
			delete(settings, "extraKnownMarketplaces")
		}
	}
	return removed
}

// claudePluginUninstall walks the `claude plugin list` output and
// uninstalls any cassandra-plugins entry. Best-effort — failures don't
// abort setup. Run after settings.json mutation so the user's preference
// state is consistent first.
func claudePluginUninstall(quiet bool) {
	if _, err := exec.LookPath("claude"); err != nil {
		return
	}
	out, err := exec.Command("claude", "plugin", "list", "--json").Output()
	if err != nil {
		return
	}
	var entries []map[string]any
	if err := json.Unmarshal(out, &entries); err != nil {
		return
	}
	for _, e := range entries {
		id, _ := e["id"].(string)
		scope, _ := e["scope"].(string)
		if !strings.HasSuffix(id, "@cassandra-plugins") {
			continue
		}
		if !quiet {
			fmt.Printf("  uninstalling legacy %s (scope: %s)...\n", id, scope)
		}
		_ = exec.Command("claude", "plugin", "uninstall", id, "--scope", scope).Run()
	}
}

// getOrMintKey returns a per-service MCP key. Reuses any cached one that
// portal still considers valid; mints a fresh one otherwise.
func getOrMintKey(client *portal.Client, projectID, service string) (string, error) {
	key := auth.GetServiceKey(service)
	if key != "" {
		if _, refresh := keyNeedsRefresh(client, key); !refresh {
			return key, nil
		}
	}
	fresh, err := mintKey(client, projectID, service)
	if err != nil {
		return "", err
	}
	if err := auth.SaveServiceKey(service, fresh, client.Email()); err != nil {
		return "", err
	}
	return fresh, nil
}

func defaultProjectID(c *portal.Client) (string, error) {
	var projects []struct {
		ID string `json:"id"`
	}
	if err := c.Get("/api/projects", &projects); err != nil {
		return "default", nil
	}
	if len(projects) > 0 {
		return projects[0].ID, nil
	}
	return "default", nil
}

// mapClaudeScope translates the user-facing flag values to the
// claudecfg.Scope enum.
func mapClaudeScope(scope string) claudecfg.Scope {
	switch scope {
	case "user":
		return claudecfg.ScopeUser
	case "project":
		return claudecfg.ScopeProject
	case "local":
		return claudecfg.ScopeLocal
	default:
		return claudecfg.ScopeUser
	}
}

// teardownClaudeDirect removes all Cassandra-managed entries from the
// scope-appropriate settings.json. Also clears cached per-service keys.
func teardownClaudeDirect(scope string, quiet bool) ([]string, error) {
	cfgScope := mapClaudeScope(scope)
	settings, err := claudecfg.LoadSettings(cfgScope)
	if err != nil {
		return nil, err
	}
	removed := []string{}
	for _, svc := range registry.Services {
		if claudecfg.RemoveMCPServer(settings, svc.Name) {
			removed = append(removed, svc.Name)
			if !quiet {
				fmt.Printf("  removed %s\n", svc.Name)
			}
		}
		if changed, _ := claudecfg.RemoveSkill(svc.Name); changed {
			if !quiet {
				fmt.Printf("  removed skill %s\n", svc.Name)
			}
		}
		_ = auth.ClearServiceKey(svc.Name)
	}
	// Also clean up the cass-managed auto-update hook + any cookie-sync
	// hooks we may have installed.
	claudecfg.RemoveAutoUpdateHook(settings)
	for _, svc := range registry.Services {
		// We don't know the manifest's cookieSync without re-fetching;
		// best effort — strip any cookie-sync hook by canonical name.
		claudecfg.RemoveCookieSyncHook(settings, svc.Name)
	}
	if err := claudecfg.SaveSettings(cfgScope, settings); err != nil {
		return removed, err
	}
	return removed, nil
}

// ensureGhAuthed verifies the gh CLI is installed and authenticated. Used
// at the top of setup since every manifest fetch needs it.
func ensureGhAuthed() error {
	if _, err := exec.LookPath("gh"); err != nil {
		return errors.New("gh CLI not found. Install: brew install gh")
	}
	out, err := exec.Command("gh", "auth", "status").CombinedOutput()
	if err != nil {
		// Best-effort: even auth status sometimes exits non-zero for
		// recoverable reasons. Look for the success marker.
		if !strings.Contains(string(out), "Logged in to github.com") {
			return fmt.Errorf("gh CLI not authenticated. Run: gh auth login\n%s", string(out))
		}
	}
	return nil
}

// cassandra-stack/.gitignore reserved path — silence unused-import noise
// if we later have to plumb filepath through this file directly.
var _ = filepath.Separator
var _ = sort.Strings
var _ = os.UserHomeDir
