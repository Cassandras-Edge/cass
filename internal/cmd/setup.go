package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"github.com/Cassandras-Edge/cass/internal/auth"
	"github.com/Cassandras-Edge/cass/internal/claudecfg"
	"github.com/Cassandras-Edge/cass/internal/portal"
	"github.com/Cassandras-Edge/cass/internal/registry"
	"github.com/Cassandras-Edge/cass/internal/shellrc"
)

// ─── codex server registry (independent of the Claude path) ────────────────
//
// Codex setup writes ~/.codex/config.toml directly with bearer tokens
// inline — no marketplace, no plugin layer. This is the model the Claude
// side now follows too via internal/manifest + internal/claudecfg.

type codexServerSpec struct {
	Service     string
	Subdomain   string
	Description string // shown only for codex-only services (others reuse registry.Service.Description)
}

var codexServers = map[string]codexServerSpec{
	"yt-mcp":          {Service: "yt-mcp", Subdomain: "youtube"},
	"discord-mcp":     {Service: "discord-mcp", Subdomain: "discord-mcp"},
	"twitter-mcp":     {Service: "twitter-mcp", Subdomain: "twitter-mcp"},
	"market-research": {Service: "market-research", Subdomain: "market-research"},
	"reddit-mcp":      {Service: "reddit-mcp", Subdomain: "reddit"},
	"claudeai-mcp":    {Service: "claudeai-mcp", Subdomain: "claude-ai"},
	"gemini-mcp":      {Service: "gemini-mcp", Subdomain: "gemini"},
	"perplexity-mcp":  {Service: "perplexity-mcp", Subdomain: "perplexity"},
	"tradingview-mcp": {Service: "tradingview-mcp", Subdomain: "tradingview-mcp"},
	"routines":        {Service: "routines", Subdomain: "routines-mcp"},
	"schwab-mcp":      {Service: "schwab-mcp", Subdomain: "schwab"},
	"gmail-mcp":       {Service: "gmail-mcp", Subdomain: "gmail-mcp"},
	"flock-mcp":   {Service: "flock-mcp", Subdomain: "flock"},
}

var defaultCodexServers = []string{
	"yt-mcp", "discord-mcp", "twitter-mcp", "market-research", "reddit-mcp", "gmail-mcp",
}

var optionalCodexServers = []string{
	"claudeai-mcp", "gemini-mcp", "perplexity-mcp", "routines",
	"tradingview-mcp", "schwab-mcp", "flock-mcp",
}

var codexScopeChoices = []string{"user", "project"}

// ─── setup ─────────────────────────────────────────────────────────────────

var scopeChoices = []string{"user", "project", "local"}

func setupCmd() *cobra.Command {
	opts := setupOptions{
		client:             "auto",
		scope:              "project",
		installAutoHook:    true,
		installShellRebind: true,
	}
	var includes []string
	var nonInteractive bool
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "First-time Cassandra setup for Claude Code, Codex, or both",
		Long: `Idempotent + interactive. Fetches each service's cass-manifest.json
from its GitHub repo, writes mcpServers entries into the chosen scope's
settings.json (Claude) or config.toml (Codex), drops SKILL.md files, mints
per-service MCP keys, and installs a SessionStart auto-update hook.

Re-running setup is the canonical way to refresh manifests + rotate keys.`,
		RunE: func(_ *cobra.Command, _ []string) error {
			if err := validateChoice(opts.client, []string{"auto", "claude", "codex", "both"}, "--client"); err != nil {
				return err
			}
			if err := validateChoice(opts.scope, scopeChoices, "--scope"); err != nil {
				return err
			}
			opts.includes = includes
			opts.nonInteractive = nonInteractive
			return runSetup(opts)
		},
	}
	cmd.Flags().StringVar(&opts.client, "client", "auto", "Which client to set up: auto | claude | codex | both")
	cmd.Flags().StringVar(&opts.scope, "scope", "project", "Settings scope: project | user | local")
	cmd.Flags().StringSliceVar(&includes, "with", nil, "Enable an optional service (repeatable, comma-separated, or 'all'). Skips the interactive picker.")
	cmd.Flags().StringVar(&opts.deviceName, "device", "", "Device name to register (default: prompt with hostname)")
	cmd.Flags().BoolVar(&opts.reauth, "reauth", false, "Force a fresh device login even if creds look valid")
	cmd.Flags().BoolVarP(&nonInteractive, "non-interactive", "y", false, "Don't prompt — use flag values as-is")
	cmd.Flags().BoolVar(&opts.installAutoHook, "auto-hook", true, "Install SessionStart hook that auto-rotates near-expiry MCP keys")
	cmd.Flags().BoolVar(&opts.installShellRebind, "shell-rebind", true, "Add alias claude='cass claude' and codex='cass codex' to ~/.zshrc (managed block)")
	return cmd
}

// setupOptions captures every selectable knob in `cass setup`. Cobra
// flags populate it with defaults; the interactive form then mutates it
// in place when the user is in a TTY and didn't pass --non-interactive.
type setupOptions struct {
	client             string
	scope              string
	deviceName         string
	includes           []string // --with values; non-empty skips interactive
	reauth             bool
	nonInteractive     bool
	installAutoHook    bool
	installShellRebind bool
}

func runSetup(opts setupOptions) error {
	fmt.Println("Checking device authorization...")
	if err := ensureDeviceAuthorized(opts.deviceName, opts.reauth); err != nil {
		return err
	}

	// Interactive form — covers client, scope, hook, and the service
	// catalog. Skipped if --with names something explicitly,
	// --non-interactive is set, or we're not on a TTY (pipes, CI, etc).
	useAllowList := false
	var allowList []string
	if len(opts.includes) == 0 && !opts.nonInteractive && isInteractive() {
		picked, newOpts, err := runSetupForm(opts)
		if err != nil {
			return err
		}
		opts = newOpts
		allowList = picked
		useAllowList = true
	}

	clients, err := resolveClients(opts.client)
	if err != nil {
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

	// Build per-client opt-in lists. In allow-list mode optInClaude/Codex
	// are the slices of OPTIONAL services the user picked; required ones
	// are passed via the allow-list mode flag so sync* can honor opt-outs.
	allOptional := union(optionalCodexServers, registryOptionalNames())
	var optInClaude, optInCodex []string
	if useAllowList {
		for _, name := range allowList {
			if contains(registryOptionalNames(), name) {
				optInClaude = append(optInClaude, name)
			}
			if contains(optionalCodexServers, name) {
				optInCodex = append(optInCodex, name)
			}
		}
	} else {
		optInClaude = resolveOptIns(opts.includes, registryOptionalNames(), allOptional)
		optInCodex = resolveOptIns(opts.includes, optionalCodexServers, allOptional)
	}

	if contains(clients, "claude") {
		fmt.Println()
		// cass setup is an explicit user action — always fetch fresh
		// manifests, ignoring the per-service 1h cache. The cache is
		// for hook-driven invocations like cass refresh-keys.
		claudeAllow := allowList
		if !useAllowList {
			claudeAllow = nil
		}
		if err := syncClaudeDirect(opts.scope, optInClaude, true, false, claudeAllow, opts.installAutoHook); err != nil {
			return err
		}
		if contains(clients, "codex") {
			fmt.Println()
		}
	}

	if contains(clients, "codex") {
		fmt.Println()
		codexAllow := allowList
		if !useAllowList {
			codexAllow = nil
		}
		syncCodex(true, optInCodex, codexScopeFor(opts.scope), codexAllow)
	}

	// Shell rebind — writes/updates the managed alias block in ~/.zshrc
	// (and ~/.bashrc if it exists). Skipped when opted out via the form
	// or `--shell-rebind=false`. Failures are logged but don't fail the
	// run since the setup itself is otherwise complete.
	if opts.installShellRebind {
		fmt.Println()
		fmt.Println("Updating shell rebind...")
		touched, err := shellrc.Install()
		switch {
		case err != nil:
			fmt.Fprintf(os.Stderr, "  warning: shell rebind: %v\n", err)
		case len(touched) > 0:
			for _, p := range touched {
				fmt.Printf("  wrote managed block to %s\n", p)
			}
			fmt.Println("  open a new shell (or `source ~/.zshrc`) so the aliases take effect.")
			if rcFile, line := shellrc.FindExistingClaudeFunction(); rcFile != "" {
				fmt.Fprintf(os.Stderr,
					"  ⚠ %s:%d defines a claude() function — alias now shadows it.\n",
					rcFile, line,
				)
				fmt.Fprintln(os.Stderr,
					"    Migrate args/env to ~/.cass/config.toml via `cass config edit --global`.")
			}
		default:
			fmt.Println("  already up to date.")
		}
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

	// flock client wiring — complementary to flock-mcp (the remote
	// claude.ai control surface). flock runs codex agents on a Mac hub and
	// loads CASS_MCP_KEY from ~/.cass/env, so the auth piece is already done by
	// login. Here we (a) sanity-check that env carries CASS_MCP_KEY, and (b) if
	// the user is sitting inside a .flock project, nudge them toward
	// `cass flock wire`. Non-intrusive: a hint, plus an optional prompt on TTY.
	maybeOfferFlockWire(useAllowList, allowList, opts.includes)

	return nil
}

// maybeOfferFlockWire surfaces the flock client integration when flock is
// relevant — i.e. flock-mcp was selected, or we're on macOS (where the
// flock hub runs). It confirms CASS_MCP_KEY is present in ~/.cass/env and, when
// the cwd is inside a .flock project, either offers to author its mcp.toml
// (interactive) or prints the `cass flock wire` hint. It never fails setup.
func maybeOfferFlockWire(useAllowList bool, allowList, includes []string) {
	flockRelevant := runtime.GOOS == "darwin" || commandExists("flock")
	if useAllowList {
		flockRelevant = flockRelevant || contains(allowList, "flock-mcp")
	} else {
		flockRelevant = flockRelevant || contains(includes, "flock-mcp") || contains(includes, "all")
	}
	if !flockRelevant {
		return
	}

	// CASS_MCP_KEY is what flock-server reads for the Cassandra fleet. It's
	// written by `cass login`; ensureDeviceAuthorized ran above so it should be
	// present, but warn (don't fail) if it isn't.
	if creds, err := auth.Read(); err != nil || creds.MCPKey == "" {
		fmt.Println()
		fmt.Fprintln(os.Stderr, "  note: ~/.cass/env has no CASS_MCP_KEY — run `cass login` so flock can auth the fleet.")
		return
	}

	cwd, _ := os.Getwd()
	root, err := findFlockProjectRoot(cwd)
	if err != nil {
		// Not inside a flock project — nothing actionable, stay quiet.
		return
	}

	fmt.Println()
	fmt.Printf("flock project detected at %s.\n", root)
	if !isInteractive() {
		fmt.Println("  Wire the Cassandra fleet into it with: cass flock wire")
		return
	}
	var doWire bool
	confirm := huh.NewConfirm().
		Title("Author this project's .flock/mcp.toml from the Cassandra fleet now?").
		Description("Runs `cass flock wire` — pick which MCP servers this project's routines can use.").
		Value(&doWire)
	if err := confirm.Run(); err != nil || !doWire {
		fmt.Println("  Skipped — run `cass flock wire` anytime to author it.")
		return
	}
	if err := runFlockWire(nil); err != nil {
		fmt.Fprintf(os.Stderr, "  warning: flock wire: %v\n", err)
	}
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
	cmd.Flags().StringVar(&scope, "scope", "project", "Settings scope to clean")
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

	// Strip the managed shell alias block. Quiet on no-op so users who
	// never installed it don't see a misleading "removed" line.
	shellTouched, err := shellrc.Remove()
	if err != nil {
		fmt.Fprintf(os.Stderr, "  warning: shellrc remove: %v\n", err)
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
	if len(shellTouched) > 0 {
		fmt.Printf("Shell rebind: stripped from %s\n", strings.Join(shellTouched, ", "))
	}
	return nil
}

// ─── claude / codex sub-groups removed ─────────────────────────────────────
//
// `cass claude` / `cass codex` are now passthrough wrappers (see wrapper.go).
// The old setup/teardown subcommands moved to:
//   - `cass setup --client claude` / `cass setup --client codex`
//   - `cass teardown --client claude` / `cass teardown --client codex`

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
	return runLogin(context.Background(), deviceName)
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
//
// allowList non-nil = the explicit set the user picked in the interactive
// picker; defaults are not auto-added and any installed-but-unpicked
// services get removed. nil = legacy defaults + opt-ins behavior.
func syncCodex(installMissing bool, optIn []string, scope string, allowList []string) {
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

	// Allow-list mode: drop any Cassandra-known codex servers the user
	// unchecked. Non-Cassandra mcp_servers entries are untouched.
	if allowList != nil {
		allowSet := map[string]bool{}
		for _, n := range allowList {
			allowSet[n] = true
		}
		for name := range codexServers {
			if !allowSet[name] {
				if removeCodexServer(cfg, name) {
					_ = auth.ClearServiceKey(codexServers[name].Service)
					fmt.Printf("Removed %s (deselected)...\n", name)
				}
			}
		}
	}

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
		var shouldInstall bool
		if allowList != nil {
			// Allow-list mode: install if and only if it's in allowList.
			for _, n := range allowList {
				if n == name {
					shouldInstall = true
					break
				}
			}
			if !shouldInstall {
				continue
			}
		} else {
			isOptional := contains(optionalCodexServers, name)
			if !exists && isOptional && !contains(optIn, name) {
				skipped = append(skipped, name)
				continue
			}
			if !exists && !installMissing {
				continue
			}
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

// isInteractive reports whether stdin AND stdout are both TTYs. Both ends
// matter — huh forms write to stdout and read from stdin, so a pipe on
// either side breaks the form and we'd hang waiting for input that never
// comes (or render gibberish into a log).
func isInteractive() bool {
	si, _ := os.Stdin.Stat()
	so, _ := os.Stdout.Stat()
	return (si.Mode()&os.ModeCharDevice) != 0 && (so.Mode()&os.ModeCharDevice) != 0
}

// serviceCatalogEntry is one row in the unified setup picker. It's the
// union of registry.Service (Claude side) and codexServers (Codex side).
type serviceCatalogEntry struct {
	Name        string
	Description string
	Required    bool // pre-checked, can still be toggled off
	Installed   bool // already present in current scope's settings/config
	Authed      bool // per-service MCP key is cached locally (no portal round-trip needed)
}

// buildServiceCatalog returns every service applicable to the active
// client(s), with descriptions sourced from registry.Service (preferred)
// or codexServers (for codex-only entries). Required defaults
// to the union of registry defaults + codex defaults across applicable
// clients.
func buildServiceCatalog(clients []string, scope string) []serviceCatalogEntry {
	type row struct {
		desc      string
		required  bool
		installed bool
	}
	rows := map[string]*row{}

	getOrCreate := func(name string) *row {
		r, ok := rows[name]
		if !ok {
			r = &row{}
			rows[name] = r
		}
		return r
	}

	if contains(clients, "claude") {
		installed := installedClaudeServices(scope)
		for _, s := range registry.Services {
			r := getOrCreate(s.Name)
			if r.desc == "" {
				r.desc = s.Description
			}
			if !s.Optional {
				r.required = true
			}
			if installed[s.Name] {
				r.installed = true
			}
		}
	}
	if contains(clients, "codex") {
		installed := installedCodexServers(scope)
		// Required = codex default set when codex is active.
		requiredSet := map[string]bool{}
		for _, n := range defaultCodexServers {
			requiredSet[n] = true
		}
		for name, spec := range codexServers {
			r := getOrCreate(name)
			if r.desc == "" {
				r.desc = spec.Description
			}
			if r.desc == "" {
				if rs := registry.Find(name); rs != nil {
					r.desc = rs.Description
				}
			}
			if requiredSet[name] {
				r.required = true
			}
			if installed[name] {
				r.installed = true
			}
		}
	}

	names := make([]string, 0, len(rows))
	for n := range rows {
		names = append(names, n)
	}
	// Sort: required first (alpha within group), then optional (alpha) — so
	// the user sees what'll install by default at the top.
	sort.Slice(names, func(i, j int) bool {
		ri, rj := rows[names[i]].required, rows[names[j]].required
		if ri != rj {
			return ri // true sorts first
		}
		return names[i] < names[j]
	})

	out := make([]serviceCatalogEntry, 0, len(names))
	for _, n := range names {
		r := rows[n]
		out = append(out, serviceCatalogEntry{
			Name:        n,
			Description: r.desc,
			Required:    r.required,
			Installed:   r.installed,
			Authed:      auth.GetServiceKey(n) != "",
		})
	}
	return out
}

func installedClaudeServices(scope string) map[string]bool {
	out := map[string]bool{}
	settings, err := claudecfg.LoadSettings(claudecfg.Scope(scope))
	if err != nil {
		return out
	}
	if servers, ok := settings["mcpServers"].(map[string]any); ok {
		for name := range servers {
			out[name] = true
		}
	}
	return out
}

func installedCodexServers(scope string) map[string]bool {
	out := map[string]bool{}
	path, err := codexConfigPath(codexScopeFor(scope))
	if err != nil {
		return out
	}
	cfg, err := loadCodexConfig(path)
	if err != nil {
		return out
	}
	if servers, ok := cfg["mcp_servers"].(map[string]any); ok {
		for name := range servers {
			out[name] = true
		}
	}
	return out
}

// dim is for the right-hand-side description and the (required) / (installed) tags.
var (
	dimStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	authedStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("36"))  // teal — has a cached key
	noAuthStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("214")) // amber — needs minting
)

// runSetupForm is the full-suite interactive setup. Two huh.Form passes:
//
//  1. Components: which client(s) to configure, which scope, and whether
//     to wire the auto-update SessionStart hook. Flag values pre-fill all
//     three fields.
//
//  2. Services: the per-service multi-select picker (existing logic).
//     Built AFTER the components pass so its catalog reflects the
//     user's just-picked client + scope.
//
// Returns the selected services + the (possibly mutated) options. Ctrl-c
// at either pass returns an error so we don't silently proceed.
func runSetupForm(opts setupOptions) ([]string, setupOptions, error) {
	// Pass 1 — components.
	// Resolve --client auto to the concrete value the user sees in the
	// form (Both / Claude / Codex) so they're not staring at "auto".
	clientChoice := opts.client
	if clientChoice == "auto" {
		// best-effort detection without erroring (resolveClients errors
		// when neither CLI is present; here we want a defaulted pick).
		hasClaude := commandExists("claude")
		hasCodex := commandExists("codex")
		switch {
		case hasClaude && hasCodex:
			clientChoice = "both"
		case hasClaude:
			clientChoice = "claude"
		case hasCodex:
			clientChoice = "codex"
		default:
			clientChoice = "both"
		}
	}

	// One question per group so huh paginates — the user sees a single
	// focused screen at a time instead of a wall of selects.
	componentsForm := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Client(s) to configure").
				Description("Which AI assistant(s) should cass write configs for?").
				Options(
					huh.NewOption("Both — Claude Code + Codex", "both"),
					huh.NewOption("Claude Code only", "claude"),
					huh.NewOption("Codex only", "codex"),
				).
				Value(&clientChoice),
		),
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Settings scope").
				Description("Project = this directory only (recommended). User = global on this machine.").
				Options(
					huh.NewOption("Project — only this directory (<cwd>/.claude, <cwd>/.codex)", "project"),
					huh.NewOption("User — global (~/.claude, ~/.codex)", "user"),
				).
				Value(&opts.scope),
		),
		huh.NewGroup(
			huh.NewConfirm().
				Title("Install auto-rotate hook?").
				Description("SessionStart hook in Claude that runs `cass refresh-keys --if-near-expiry` so MCP keys self-heal.").
				Value(&opts.installAutoHook),
		),
		huh.NewGroup(
			huh.NewConfirm().
				Title("Rebind claude/codex to cass?").
				Description(shellRebindDescription()).
				Value(&opts.installShellRebind),
		),
	)

	if err := componentsForm.Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return nil, opts, fmt.Errorf("setup aborted")
		}
		return nil, opts, err
	}
	opts.client = clientChoice

	// Pass 2 — services picker. Build catalog based on the just-picked
	// client(s) so claude-only/codex-only entries filter correctly.
	clients, err := resolveClientsLenient(opts.client)
	if err != nil {
		return nil, opts, err
	}
	picked, err := promptServiceSelection(clients, opts.scope)
	if err != nil {
		return nil, opts, err
	}
	return picked, opts, nil
}

// resolveClientsLenient is like resolveClients but allows clients to be
// missing from PATH (the user picked them in the form, presumably knowing
// what they want installed — let the downstream commands surface any real
// "not installed" errors).
func resolveClientsLenient(client string) ([]string, error) {
	switch client {
	case "both":
		return []string{"claude", "codex"}, nil
	case "claude", "codex":
		return []string{client}, nil
	case "auto":
		return resolveClients("auto")
	}
	return nil, fmt.Errorf("unknown client: %s", client)
}

func commandExists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// shellRebindDescription builds the help text shown beneath the rebind
// confirm in the setup form. When the user already has a hand-written
// `claude()` function in their rc, we flag it so they aren't surprised
// when the alias shadows their custom logic.
func shellRebindDescription() string {
	base := "Adds `alias claude='cass claude'` and `alias codex='cass codex'` to ~/.zshrc (managed block — `cass teardown` removes it cleanly)."
	if rcFile, line := shellrc.FindExistingClaudeFunction(); rcFile != "" {
		base += fmt.Sprintf("\n⚠ %s:%d defines a claude() function — the alias will shadow it. Migrate args/env to ~/.cass/config.toml first.", rcFile, line)
	}
	return base
}

// promptServiceSelection shows every service applicable to the active
// client(s) in one multi-select with descriptions. Required + already-
// installed services start checked; the user can toggle anything off to
// opt out. Returns the explicit set of service names to install.
//
// Aborting via ctrl-c returns an error so we don't silently proceed.
func promptServiceSelection(clients []string, scope string) ([]string, error) {
	catalog := buildServiceCatalog(clients, scope)
	if len(catalog) == 0 {
		return nil, nil
	}

	// Pad the name column so descriptions line up.
	nameWidth := 0
	for _, e := range catalog {
		if len(e.Name) > nameWidth {
			nameWidth = len(e.Name)
		}
	}

	opts := make([]huh.Option[string], 0, len(catalog))
	selected := make([]string, 0, len(catalog))
	for _, e := range catalog {
		tag := ""
		switch {
		case e.Required:
			tag = " (required)"
		case e.Installed:
			tag = " (installed)"
		}
		// Auth indicator on the right — green dot if cached MCP key
		// exists for this service, amber dot if a fresh mint is needed.
		var authMark string
		if e.Authed {
			authMark = authedStyle.Render("● authed")
		} else {
			authMark = noAuthStyle.Render("○ needs auth")
		}
		label := fmt.Sprintf("%-*s  %s   %s",
			nameWidth, e.Name,
			dimStyle.Render(e.Description+tag),
			authMark,
		)
		opt := huh.NewOption(label, e.Name)
		if e.Required || e.Installed {
			opt = opt.Selected(true)
			selected = append(selected, e.Name)
		}
		opts = append(opts, opt)
	}

	form := huh.NewMultiSelect[string]().
		Title("Cassandra services").
		Description("Space to toggle, enter to confirm. Required ones start checked; uncheck to opt out.").
		Options(opts...).
		Value(&selected).
		Height(len(catalog) + 4)

	if err := form.Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return nil, fmt.Errorf("setup aborted")
		}
		return nil, err
	}
	return selected, nil
}
