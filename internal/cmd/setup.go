package cmd

import (
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
	"github.com/Cassandras-Edge/cass/internal/registry"
)

// ─── codex server registry (independent of the Claude path) ────────────────
//
// Codex setup writes ~/.codex/config.toml directly with bearer tokens
// inline — no marketplace, no plugin layer. This is the model the Claude
// side now follows too via internal/manifest + internal/claudecfg.

type codexServerSpec struct {
	Service   string
	Subdomain string
}

var codexServers = map[string]codexServerSpec{
	"yt-mcp":          {"yt-mcp", "youtube"},
	"discord-mcp":     {"discord-mcp", "discord-mcp"},
	"twitter-mcp":     {"twitter-mcp", "twitter-mcp"},
	"market-research": {"market-research", "market-research"},
	"reddit-mcp":      {"reddit-mcp", "reddit"},
	"claudeai-mcp":    {"claudeai-mcp", "claude-ai"},
	"gemini-mcp":      {"gemini-mcp", "gemini"},
	"perplexity-mcp":  {"perplexity-mcp", "perplexity"},
	"gateway":         {"gateway", "gateway"},
	"tradingview-mcp": {"tradingview-mcp", "tradingview-mcp"},
	"routines":        {"routines", "routines-mcp"},
	"schwab-mcp":      {"schwab-mcp", "schwab"},
	"gmail-mcp":       {"gmail-mcp", "gmail-mcp"},
}

var defaultCodexServers = []string{
	"yt-mcp", "discord-mcp", "twitter-mcp", "market-research", "reddit-mcp", "gmail-mcp",
}

var optionalCodexServers = []string{
	"claudeai-mcp", "gemini-mcp", "perplexity-mcp", "gateway", "routines",
	"tradingview-mcp", "schwab-mcp",
}

var codexScopeChoices = []string{"user", "project"}

// ─── setup ─────────────────────────────────────────────────────────────────

var scopeChoices = []string{"user", "project", "local"}

func setupCmd() *cobra.Command {
	var client, scope, deviceName string
	var includes []string
	var reauth bool
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "First-time Cassandra setup for Claude Code, Codex, or both",
		Long: `Idempotent + interactive. Fetches each service's cass-manifest.json
from its GitHub repo, writes mcpServers entries into the chosen scope's
settings.json (Claude) or config.toml (Codex), drops SKILL.md files, mints
per-service MCP keys, and installs a SessionStart auto-update hook.

Re-running setup is the canonical way to refresh manifests + rotate keys.`,
		RunE: func(_ *cobra.Command, _ []string) error {
			if err := validateChoice(client, []string{"auto", "claude", "codex", "both"}, "--client"); err != nil {
				return err
			}
			if err := validateChoice(scope, scopeChoices, "--scope"); err != nil {
				return err
			}
			return runSetup(client, scope, includes, deviceName, reauth)
		},
	}
	cmd.Flags().StringVar(&client, "client", "auto", "Which client to set up: auto | claude | codex | both")
	cmd.Flags().StringVar(&scope, "scope", "user", "Claude settings scope: user | project | local")
	cmd.Flags().StringSliceVar(&includes, "with", nil, "Enable an optional service (repeatable, comma-separated, or 'all')")
	cmd.Flags().StringVar(&deviceName, "device", "", "Device name to register (default: prompt with hostname)")
	cmd.Flags().BoolVar(&reauth, "reauth", false, "Force a fresh device login even if creds look valid")
	return cmd
}

func runSetup(client, scope string, includes []string, deviceName string, reauth bool) error {
	clients, err := resolveClients(client)
	if err != nil {
		return err
	}

	fmt.Println("Checking device authorization...")
	if err := ensureDeviceAuthorized(deviceName, reauth); err != nil {
		return err
	}

	// gh auth is required for manifest fetches on the Claude path.
	if contains(clients, "claude") {
		if err := ensureGhAuthed(); err != nil {
			return err
		}
	}

	// One-shot legacy cleanup: uninstall any cassandra-plugins entries
	// `claude plugin install` left behind.
	if contains(clients, "claude") {
		claudePluginUninstall(false)
	}

	allOptional := union(optionalCodexServers, registryOptionalNames())
	optInClaude := resolveOptIns(includes, registryOptionalNames(), allOptional)
	optInCodex := resolveOptIns(includes, optionalCodexServers, allOptional)

	if contains(clients, "claude") {
		fmt.Println()
		if err := syncClaudeDirect(scope, optInClaude, false, false); err != nil {
			return err
		}
		if contains(clients, "codex") {
			fmt.Println()
		}
	}

	if contains(clients, "codex") {
		fmt.Println()
		fmt.Println("Updating patched Claude CLI...")
		if err := installPatchedPrebuilt(""); err != nil {
			fmt.Fprintf(os.Stderr, "  warning: patched-cli install: %v\n", err)
		}
		syncCodex(true, optInCodex, codexScopeFor(scope))
	}

	fmt.Println()
	if contains(clients, "claude") {
		fmt.Println("Claude services (default):")
		for _, s := range registry.Defaults() {
			fmt.Printf("  - %s\n", s.Name)
		}
		if len(registry.Optionals()) > 0 {
			fmt.Println("Optional (opt in with --with <name>):")
			for _, s := range registry.Optionals() {
				mark := " "
				if contains(optInClaude, s.Name) {
					mark = "✓"
				}
				fmt.Printf("  %s %s\n", mark, s.Name)
			}
		}
		fmt.Println()
		fmt.Println("Restart Claude Code to pick up the new MCP servers + skills.")
	}
	if contains(clients, "codex") {
		if contains(clients, "claude") {
			fmt.Println()
		}
		fmt.Println("Restart Codex to activate the MCP servers (no env sourcing required).")
	}
	return nil
}

// ─── teardown ──────────────────────────────────────────────────────────────

func teardownCmd() *cobra.Command {
	var client, scope string
	var yes bool
	cmd := &cobra.Command{
		Use:   "teardown",
		Short: "Remove Cassandra MCP servers from settings (inverse of `cass setup`)",
		Long: `Removes settings.json/config.toml entries cass installed. Skill files
under ~/.claude/skills/ and cached service keys are also dropped. cass
itself is not uninstalled.`,
		RunE: func(_ *cobra.Command, _ []string) error {
			if err := validateChoice(client, []string{"auto", "claude", "codex", "both"}, "--client"); err != nil {
				return err
			}
			if err := validateChoice(scope, scopeChoices, "--scope"); err != nil {
				return err
			}
			return runTeardown(client, scope, yes)
		},
	}
	cmd.Flags().StringVar(&client, "client", "auto", "Which client to tear down: auto | claude | codex | both")
	cmd.Flags().StringVar(&scope, "scope", "user", "Claude settings scope to clean")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Skip confirmation prompt")
	return cmd
}

func runTeardown(client, scope string, assumeYes bool) error {
	clients, err := resolveClients(client)
	if err != nil {
		return err
	}
	if !assumeYes {
		var targets []string
		if contains(clients, "claude") {
			targets = append(targets, fmt.Sprintf("Claude services (scope: %s)", scope))
		}
		if contains(clients, "codex") {
			targets = append(targets, "Codex MCP servers (global)")
		}
		fmt.Printf("This will remove: %s.\n", strings.Join(targets, ", "))
		fmt.Print("Proceed? [y/N] ")
		var resp string
		fmt.Scanln(&resp)
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(resp)), "y") {
			fmt.Println("Aborted.")
			return nil
		}
	}

	var removedClaude, removedCodex []string
	if contains(clients, "claude") {
		removedClaude, _ = teardownClaudeDirect(scope, false)
		// Also clean up any legacy plugin installs.
		claudePluginUninstall(false)
		if contains(clients, "codex") {
			fmt.Println()
		}
	}
	if contains(clients, "codex") {
		removedCodex = teardownCodex(codexScopeFor(scope))
	}
	fmt.Println()
	if contains(clients, "claude") {
		fmt.Printf("Claude: removed %d service(s)", len(removedClaude))
		if len(removedClaude) > 0 {
			fmt.Printf(" — %s", strings.Join(removedClaude, ", "))
		}
		fmt.Println()
	}
	if contains(clients, "codex") {
		fmt.Printf("Codex: removed %d MCP server(s)", len(removedCodex))
		if len(removedCodex) > 0 {
			fmt.Printf(" — %s", strings.Join(removedCodex, ", "))
		}
		fmt.Println()
	}
	return nil
}

// ─── claude / codex sub-groups ─────────────────────────────────────────────

func claudeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "claude",
		Short: "Claude-specific Cassandra commands",
	}
	cmd.AddCommand(claudeSetupCmd())
	cmd.AddCommand(claudeTeardownCmd())
	return cmd
}

func claudeSetupCmd() *cobra.Command {
	var scope string
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Set up Cassandra MCP servers + skills in Claude Code settings",
		RunE: func(_ *cobra.Command, _ []string) error {
			if err := validateChoice(scope, scopeChoices, "--scope"); err != nil {
				return err
			}
			return runSetup("claude", scope, nil, "", false)
		},
	}
	cmd.Flags().StringVar(&scope, "scope", "user", "Settings scope")
	return cmd
}

func claudeTeardownCmd() *cobra.Command {
	var scope string
	var yes bool
	cmd := &cobra.Command{
		Use:   "teardown",
		Short: "Remove Cassandra entries from Claude Code settings",
		RunE: func(_ *cobra.Command, _ []string) error {
			if err := validateChoice(scope, scopeChoices, "--scope"); err != nil {
				return err
			}
			return runTeardown("claude", scope, yes)
		},
	}
	cmd.Flags().StringVar(&scope, "scope", "user", "Settings scope to remove from")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Skip confirmation prompt")
	return cmd
}

func codexCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "codex",
		Short: "Codex-specific Cassandra commands",
	}
	cmd.AddCommand(codexSetupCmd())
	cmd.AddCommand(codexTeardownCmd())
	return cmd
}

func codexSetupCmd() *cobra.Command {
	var includes []string
	var scope string
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Set up Codex MCP servers and auth for Cassandra services",
		RunE: func(_ *cobra.Command, _ []string) error {
			if err := validateChoice(scope, codexScopeChoices, "--scope"); err != nil {
				return err
			}
			return runSetup("codex", scope, includes, "", false)
		},
	}
	cmd.Flags().StringSliceVar(&includes, "with", nil, "Enable an optional server (repeatable, comma-separated, or 'all')")
	cmd.Flags().StringVar(&scope, "scope", "user", "Codex config scope: user (~/.codex/config.toml) | project (<cwd>/.codex/config.toml)")
	return cmd
}

func codexTeardownCmd() *cobra.Command {
	var yes bool
	var scope string
	cmd := &cobra.Command{
		Use:   "teardown",
		Short: "Remove Cassandra Codex MCP servers",
		RunE: func(_ *cobra.Command, _ []string) error {
			if err := validateChoice(scope, codexScopeChoices, "--scope"); err != nil {
				return err
			}
			return runTeardown("codex", scope, yes)
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Skip confirmation prompt")
	cmd.Flags().StringVar(&scope, "scope", "user", "Codex config scope to remove from: user | project")
	return cmd
}

// ─── shared internals ──────────────────────────────────────────────────────

func resolveClients(client string) ([]string, error) {
	var clients []string
	switch client {
	case "both":
		clients = []string{"claude", "codex"}
	case "auto":
		if _, err := exec.LookPath("claude"); err == nil {
			clients = append(clients, "claude")
		}
		if _, err := exec.LookPath("codex"); err == nil {
			clients = append(clients, "codex")
		}
	default:
		clients = []string{client}
	}
	if len(clients) == 0 {
		return nil, errors.New("neither claude nor codex CLI found in PATH")
	}
	for _, c := range clients {
		if _, err := exec.LookPath(c); err != nil {
			return nil, fmt.Errorf("required CLI not found: %s", c)
		}
	}
	return clients, nil
}

func registryDefaultNames() []string {
	out := []string{}
	for _, s := range registry.Defaults() {
		out = append(out, s.Name)
	}
	return out
}

func registryOptionalNames() []string {
	out := []string{}
	for _, s := range registry.Optionals() {
		out = append(out, s.Name)
	}
	return out
}

func resolveOptIns(includes, optionalPool, knownPool []string) []string {
	selected := map[string]bool{}
	for _, raw := range includes {
		for _, piece := range strings.Split(raw, ",") {
			name := strings.TrimSpace(piece)
			if name == "" {
				continue
			}
			if name == "all" {
				for _, p := range optionalPool {
					selected[p] = true
				}
				continue
			}
			if !contains(knownPool, name) {
				continue
			}
			if contains(optionalPool, name) {
				selected[name] = true
			}
		}
	}
	out := make([]string, 0, len(selected))
	for k := range selected {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// expiryWarnDays is the threshold at which `cass setup` proactively re-auths.
// Matches Python: the per-device CF service token lives 90 days, so refreshing
// within 7 days of expiry keeps users from ever hitting "stale creds" mid-flow.
const expiryWarnDays = 7

func ensureDeviceAuthorized(deviceName string, forceReauth bool) error {
	creds, err := auth.Read()
	needsLogin := forceReauth || err != nil || creds.MCPKey == ""

	if !needsLogin {
		if client, perr := portal.NewClient(); perr == nil {
			if w, werr := client.Whoami(); werr == nil {
				if expiringSoon(w.ExpiresAt) {
					fmt.Println("  device creds expire within", expiryWarnDays, "days — re-authorizing to refresh")
					needsLogin = true
				} else {
					fmt.Printf("  device creds present and healthy (email: %s)\n", creds.Email)
				}
			} else if errors.Is(werr, portal.ErrKeyNotValid) {
				fmt.Println("  existing creds rejected (revoked/expired) — re-authorizing")
				needsLogin = true
			} else {
				fmt.Fprintf(os.Stderr, "  could not reach portal to validate creds — proceeding with whatever's in env: %v\n", werr)
			}
		}
	}

	if !needsLogin {
		return nil
	}

	if deviceName == "" {
		host, _ := os.Hostname()
		host, _, _ = strings.Cut(host, ".")
		deviceName = host
	}
	fmt.Printf("  authorizing device '%s'...\n", deviceName)
	return runLogin(nil, deviceName)
}

func expiringSoon(expiresAt string) bool {
	if expiresAt == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, expiresAt)
	if err != nil {
		t, err = time.Parse("2006-01-02T15:04:05.000Z", expiresAt)
		if err != nil {
			return false
		}
	}
	return time.Until(t) <= time.Duration(expiryWarnDays)*24*time.Hour
}

// codexScopeFor maps the shared --scope flag to a codex-valid scope. Codex
// only understands user / project, so Claude's "local" collapses to project.
func codexScopeFor(scope string) string {
	switch scope {
	case "user":
		return "user"
	case "project", "local":
		return "project"
	default:
		return "user"
	}
}

// syncCodex mints per-service MCP keys (same flow as `cass refresh-keys`)
// and writes them inline as `http_headers.Authorization = "Bearer <key>"`
// in either ~/.codex/config.toml (scope=user) or <cwd>/.codex/config.toml
// (scope=project). Codex picks them up at startup with no env sourcing.
func syncCodex(installMissing bool, optIn []string, scope string) {
	fmt.Printf("Syncing Codex MCP servers (scope: %s)...\n", scope)

	cfgPath, err := codexConfigPath(scope)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  warning: %v\n", err)
		return
	}
	cfg, err := loadCodexConfig(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  warning: %v\n", err)
		return
	}

	client, perr := portal.NewClient()
	if perr != nil {
		fmt.Fprintf(os.Stderr, "  warning: %v (run `cass login`)\n", perr)
		return
	}
	projectID, _ := defaultProjectID(client)

	existingServers, _ := cfg["mcp_servers"].(map[string]any)

	var touched, skipped []string
	names := make([]string, 0, len(codexServers))
	for k := range codexServers {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, name := range names {
		meta := codexServers[name]
		_, exists := existingServers[name]
		isOptional := contains(optionalCodexServers, name)
		if !exists && isOptional && !contains(optIn, name) {
			skipped = append(skipped, name)
			continue
		}
		if !exists && !installMissing {
			continue
		}

		key := auth.GetServiceKey(meta.Service)
		needsMint := key == ""
		if !needsMint {
			if reason, refresh := keyNeedsRefresh(client, key); refresh {
				fmt.Printf("Re-minting %s key (%s)...\n", name, reason)
				needsMint = true
			}
		}
		if needsMint {
			fmt.Printf("Minting key for %s...\n", meta.Service)
			fresh, err := mintKey(client, projectID, meta.Service)
			if err != nil {
				fmt.Fprintf(os.Stderr, "  warning: mint %s: %v\n", meta.Service, err)
				continue
			}
			if err := auth.SaveServiceKey(meta.Service, fresh, client.Email()); err != nil {
				fmt.Fprintf(os.Stderr, "  warning: cache %s: %v\n", meta.Service, err)
				continue
			}
			key = fresh
		}

		fmt.Printf("Configuring %s → %s.cassandrasedge.com...\n", name, meta.Subdomain)
		upsertCodexHTTPServer(cfg, name, codexHTTPServer{
			URL: codexURL(meta.Subdomain),
			HTTPHeaders: map[string]string{
				"Authorization": "Bearer " + key,
			},
		})
		touched = append(touched, name)
	}

	if len(skipped) > 0 {
		fmt.Println()
		fmt.Printf("Skipped optional: %s — enable with `cass codex setup --with <name>`.\n", strings.Join(skipped, ", "))
	}
	if len(touched) == 0 {
		fmt.Println()
		fmt.Println("No Cassandra Codex MCP servers configured. Run `cass codex setup` to add them.")
		return
	}

	if err := saveCodexConfig(cfgPath, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "  warning: write %s: %v\n", cfgPath, err)
		return
	}

	if scope == "project" {
		projectRoot := filepath.Dir(filepath.Dir(cfgPath))
		if err := ensureCodexProjectTrust(projectRoot); err != nil {
			fmt.Fprintf(os.Stderr, "  warning: trust project: %v\n", err)
		}
		if err := ensureCodexGitignore(projectRoot); err != nil {
			fmt.Fprintf(os.Stderr, "  warning: gitignore: %v\n", err)
		}
	}

	fmt.Println()
	fmt.Printf("Configured %d Codex MCP server(s) in %s.\n", len(touched), cfgPath)
	fmt.Println("No env-var sourcing needed — bearer tokens live inline. Restart Codex to pick up the new config.")
}

func codexURL(subdomain string) string {
	return "https://" + subdomain + ".cassandrasedge.com/mcp"
}

// teardownCodex strips Cassandra mcp_servers entries from the scope-appropriate
// codex config.toml. Other entries (codanna, context7, …) are untouched.
// We also drop the cached per-service keys so the next setup re-mints fresh.
func teardownCodex(scope string) []string {
	cfgPath, err := codexConfigPath(scope)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  warning: %v\n", err)
		return nil
	}
	cfg, err := loadCodexConfig(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  warning: %v\n", err)
		return nil
	}
	var removed []string
	for name, meta := range codexServers {
		if removeCodexServer(cfg, name) {
			fmt.Printf("Removing %s...\n", name)
			removed = append(removed, name)
			_ = auth.ClearServiceKey(meta.Service)
		}
	}
	if len(removed) == 0 {
		return nil
	}
	if err := saveCodexConfig(cfgPath, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "  warning: write %s: %v\n", cfgPath, err)
	}
	return removed
}

// installPatchedPrebuilt downloads the latest claude-patched binary for
// the host platform. Same logic as `cass patched-cli install`, kept here
// so setup can call it directly.
func installPatchedPrebuilt(releaseTag string) error {
	target, err := patchedHostTarget()
	if err != nil {
		return err
	}
	if _, err := exec.LookPath("gh"); err != nil {
		return errors.New("gh CLI not found (skipping patched-cli install)")
	}
	binPath := patchedBinPath()
	if err := os.MkdirAll(filepath.Dir(binPath), 0o755); err != nil {
		return err
	}
	if err := os.Remove(binPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	args := []string{"release", "download"}
	if releaseTag != "" {
		args = append(args, "--tag", releaseTag)
	}
	args = append(args, "--repo", ccPatchesRepo, "--pattern", "claude-patched-"+target, "--output", binPath)
	out, err := exec.Command("gh", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh release download: %w\n%s", err, string(out))
	}
	return os.Chmod(binPath, 0o755)
}

// ─── helpers ───────────────────────────────────────────────────────────────

func validateChoice(value string, choices []string, flagName string) error {
	for _, c := range choices {
		if value == c {
			return nil
		}
	}
	return fmt.Errorf("%s must be one of %s", flagName, strings.Join(choices, " | "))
}

func contains(list []string, target string) bool {
	for _, s := range list {
		if s == target {
			return true
		}
	}
	return false
}

func union(a, b []string) []string {
	seen := map[string]bool{}
	for _, s := range a {
		seen[s] = true
	}
	for _, s := range b {
		seen[s] = true
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
