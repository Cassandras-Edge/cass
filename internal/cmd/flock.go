package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"

	"github.com/Cassandras-Edge/cass/internal/registry"
)

// flockCmd is a passthrough wrapper around the local `flock` binary — exactly
// like `cass claude` / `cass codex` (see wrapper.go) — with ONE in-process
// subcommand grafted on: `cass flock wire`, which authors the current flock
// project's .flock/mcp.toml from the Cassandra fleet.
//
// Why not a normal cobra child command? A pure passthrough wrapper sets
// DisableFlagParsing so it can hand every flag/arg to the wrapped CLI
// untouched. That swallows would-be subcommands too, so a real `wire` child
// would never get a chance to run. We resolve this by keeping
// DisableFlagParsing:true and dispatching on args[0] ourselves: "wire" runs
// the wire logic, anything else (including --help/help) execs the real
// `flock` binary via runWrapper. `flock` itself renders its own help, so the
// passthrough gives the user something useful for `cass flock --help`.
func flockCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "flock [args...]",
		Short: "Run flock with per-project .cass.toml defaults (+ `cass flock wire`)",
		Long: `Passthrough wrapper around the local flock CLI: loads .cass.toml for the
cwd, auto-refreshes near-expiry MCP keys, then execs flock with the
config-derived args. Adds one cass-native subcommand:

  cass flock wire [services...]   author .flock/mcp.toml from the Cassandra fleet

Everything else is forwarded verbatim to the flock binary.`,
		DisableFlagParsing: true, // pass all flags through to the wrapped CLI
		SilenceUsage:       true,
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) > 0 && args[0] == "wire" {
				return runFlockWire(args[1:])
			}
			return runWrapper("flock", args)
		},
	}
}

// ─── flock wire ──────────────────────────────────────────────────────────────

// flockControlServer is the always-present local control server entry. It lets
// runs self-manage the flock runtime (list/fire automations, read transcripts)
// over the loopback MCP-control endpoint. No bearer — it's local + trusted.
const (
	flockControlName = "flock"
	flockControlURL  = "http://127.0.0.1:8787/mcp-control"
)

// flockWireExclude is the set of codexServers entries that must NOT be offered
// to `cass flock wire`. flock-mcp is the REMOTE claude.ai-facing control
// surface (flock.cassandrasedge.com) — it isn't a platform-provisioned fleet
// key (ensureServiceKey 400s on it), and a local flock project already gets the
// loopback control server (flockControlName) wired in unconditionally. Offering
// it would only produce a dead picker row + a scary 400 on a wrong selection.
var flockWireExclude = map[string]bool{"flock-mcp": true}

// isFlockWirable reports whether a name is a Cassandra fleet service that
// `cass flock wire` may wire (known in codexServers and not excluded).
func isFlockWirable(name string) bool {
	if flockWireExclude[name] {
		return false
	}
	_, ok := codexServers[name]
	return ok
}

// flockMarkerDir / flockProjectFile mirror flock's on-disk layout
// (flock/internal/routine: MarkerDir=".flock", ProjectFile="flock.toml",
// MCPFile="mcp.toml"). Duplicated here because flock is a separate Go module —
// cass can't import it.
const (
	flockMarkerDir   = ".flock"
	flockProjectFile = "flock.toml"
	flockMCPFile     = "mcp.toml"
)

// mcpServerEntry is cass's view of one .flock/mcp.toml server. It is the
// hand-emit mirror of flock's routine.MCPServerToml (url/bearer_env for HTTP,
// command/args for stdio). cass only ever WRITES http+bearer_env entries for
// the Cassandra fleet, but the full shape is preserved so non-Cassandra
// servers (local stdio, OAuth) round-trip untouched on re-wire.
type mcpServerEntry struct {
	URL       string
	BearerEnv string
	Command   string
	Args      []string
}

func runFlockWire(serviceArgs []string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	root, err := findFlockProjectRoot(cwd)
	if err != nil {
		return fmt.Errorf("no flock project (%s/%s) found from %s upward — run `flock connect` first",
			flockMarkerDir, flockProjectFile, cwd)
	}

	mcpPath := filepath.Join(root, flockMarkerDir, flockMCPFile)
	existing, err := parseFlockMCP(mcpPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", mcpPath, err)
	}

	// Which Cassandra fleet servers are already wired (so the interactive
	// picker pre-checks them). A fleet entry is any existing server whose name
	// matches a known fleet service.
	preselected := map[string]bool{}
	for name := range existing {
		if isFlockWirable(name) {
			preselected[name] = true
		}
	}

	// Resolve the selection: explicit args (non-interactive) or a checklist.
	var selected []string
	if len(serviceArgs) > 0 {
		for _, raw := range serviceArgs {
			name := strings.TrimSpace(raw)
			if name == "" {
				continue
			}
			if !isFlockWirable(name) {
				return fmt.Errorf("%q is not a wirable Cassandra service (run `cass flock wire` with no args to pick from the list)", name)
			}
			selected = append(selected, name)
		}
	} else {
		selected, err = promptFlockServices(preselected)
		if err != nil {
			return err
		}
	}

	// Ensure each selected service's MCP key is minted on the platform. We do
	// NOT write the token into the file — flock resolves CASS_MCP_KEY from its
	// own env (cass writes it to ~/.cass/env on login). This call is purely to
	// guarantee the platform has provisioned the key so the server works on
	// first run.
	for _, name := range selected {
		spec := codexServers[name]
		if _, _, err := ensureServiceKey(spec.Service, true); err != nil {
			fmt.Fprintf(os.Stderr, "  warning: ensure key for %s: %v\n", spec.Service, err)
		}
	}

	// Merge: start from existing (preserving non-Cassandra servers), drop any
	// fleet servers that were deselected, then (re)write the selected fleet
	// servers + the always-on flock control server. Idempotent across re-runs.
	merged := map[string]mcpServerEntry{}
	for name, entry := range existing {
		if _, isFleet := codexServers[name]; isFleet {
			// Fleet servers are authoritative from this run — only keep if
			// still selected (re-emitted below with canonical url/bearer_env).
			continue
		}
		if name == flockControlName {
			// Re-emitted canonically below.
			continue
		}
		merged[name] = entry // non-Cassandra server — preserve verbatim.
	}
	for _, name := range selected {
		spec := codexServers[name]
		merged[name] = mcpServerEntry{
			URL:       flockServiceURL(spec.Subdomain),
			BearerEnv: "CASS_MCP_KEY",
		}
	}
	// Always include the local flock control server so runs can self-manage.
	merged[flockControlName] = mcpServerEntry{URL: flockControlURL}

	if err := writeFlockMCP(mcpPath, merged); err != nil {
		return fmt.Errorf("write %s: %w", mcpPath, err)
	}

	// Summary.
	fmt.Printf("Wired %d Cassandra service(s) into %s:\n", len(selected), mcpPath)
	for _, name := range selected {
		spec := codexServers[name]
		fmt.Printf("  ✓ %-16s %s\n", name, flockServiceURL(spec.Subdomain))
	}
	fmt.Printf("  ✓ %-16s %s (local control)\n", flockControlName, flockControlURL)
	preserved := 0
	for name := range merged {
		if _, isFleet := codexServers[name]; isFleet {
			continue
		}
		if name == flockControlName {
			continue
		}
		preserved++
	}
	if preserved > 0 {
		fmt.Printf("  (preserved %d non-Cassandra server entr%s)\n", preserved, plural(preserved, "y", "ies"))
	}
	fmt.Println()
	fmt.Println("All Cassandra servers auth via $CASS_MCP_KEY (sourced from ~/.cass/env by flock-server).")
	fmt.Println("Run `flock apply` (or restart flock-server) to pick up the new servers.")
	return nil
}

// promptFlockServices shows a huh multi-select of the Cassandra fleet, sourced
// from the codexServers subdomain map (joined with registry descriptions where
// available). Entries already present in .flock/mcp.toml start checked.
func promptFlockServices(preselected map[string]bool) ([]string, error) {
	if !isInteractive() {
		return nil, errors.New("no services given and not a TTY: pass service names, e.g. `cass flock wire market-research twitter-mcp`")
	}

	// Build a stable, sorted catalog of the fleet from codexServers (the
	// authoritative subdomain source for URL derivation).
	names := make([]string, 0, len(codexServers))
	for name := range codexServers {
		if !isFlockWirable(name) {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)

	nameWidth := 0
	for _, n := range names {
		if len(n) > nameWidth {
			nameWidth = len(n)
		}
	}

	opts := make([]huh.Option[string], 0, len(names))
	selected := make([]string, 0, len(names))
	for _, name := range names {
		desc := registryDescription(name)
		label := fmt.Sprintf("%-*s  %s", nameWidth, name, dimStyle.Render(desc))
		opt := huh.NewOption(strings.TrimRight(label, " "), name)
		if preselected[name] {
			opt = opt.Selected(true)
			selected = append(selected, name)
		}
		opts = append(opts, opt)
	}

	form := huh.NewMultiSelect[string]().
		Title("Wire Cassandra MCP servers into this flock project").
		Description("Space to toggle, enter to confirm. All auth via $CASS_MCP_KEY.").
		Options(opts...).
		Value(&selected).
		Height(len(names) + 4)

	if err := form.Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return nil, errors.New("flock wire aborted")
		}
		return nil, err
	}
	return selected, nil
}

// registryDescription returns the one-line description for a fleet service,
// preferring the codexServers spec then registry.Service.
func registryDescription(name string) string {
	if spec, ok := codexServers[name]; ok && spec.Description != "" {
		return spec.Description
	}
	if rs := registry.Find(name); rs != nil {
		return rs.Description
	}
	return ""
}

// flockServiceURL derives a fleet MCP endpoint from its subdomain. Mirrors
// codexURL — kept separate so the two paths stay decoupled.
func flockServiceURL(subdomain string) string {
	return "https://" + subdomain + ".cassandrasedge.com/mcp"
}

// findFlockProjectRoot walks up from start looking for .flock/flock.toml,
// returning the absolute path of the containing folder. Mirrors
// flock/internal/routine.FindRoot (separate module — can't import).
func findFlockProjectRoot(start string) (string, error) {
	abs, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	for {
		marker := filepath.Join(abs, flockMarkerDir, flockProjectFile)
		if info, err := os.Stat(marker); err == nil && !info.IsDir() {
			return abs, nil
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return "", errors.New("not found")
		}
		abs = parent
	}
}

// ─── .flock/mcp.toml hand-emit + parse ───────────────────────────────────────
//
// flock's mcp.toml is a flat table-of-tables (`[servers.<name>]` with
// url/bearer_env/command/args). cass doesn't depend on BurntSushi/toml (flock's
// encoder), so we hand-emit deterministically and hand-parse just enough to
// round-trip non-Cassandra entries. This avoids adding a TOML-flavor dependency
// for what is a trivial, well-known schema.

// parseFlockMCP reads .flock/mcp.toml into a name→entry map. A missing file is
// not an error (empty map). Only the fields cass cares about are parsed; the
// schema is flat and fully captured by url/bearer_env/command/args.
func parseFlockMCP(path string) (map[string]mcpServerEntry, error) {
	out := map[string]mcpServerEntry{}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	var cur string
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[servers.") && strings.HasSuffix(line, "]") {
			cur = strings.TrimSuffix(strings.TrimPrefix(line, "[servers."), "]")
			cur = strings.Trim(cur, "\"")
			if _, ok := out[cur]; !ok {
				out[cur] = mcpServerEntry{}
			}
			continue
		}
		if line == "[servers]" {
			cur = ""
			continue
		}
		// Any other top-level table ([something-else]) ends the servers block.
		if strings.HasPrefix(line, "[") && !strings.HasPrefix(line, "[servers.") {
			cur = ""
			continue
		}
		if cur == "" {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		entry := out[cur]
		switch key {
		case "url":
			entry.URL = tomlUnquote(val)
		case "bearer_env":
			entry.BearerEnv = tomlUnquote(val)
		case "command":
			entry.Command = tomlUnquote(val)
		case "args":
			entry.Args = parseTOMLStringArray(val)
		}
		out[cur] = entry
	}
	return out, nil
}

// writeFlockMCP hand-emits .flock/mcp.toml from servers, deterministically
// (sorted by name) and with a managed header matching flock's own format. It
// creates the .flock/ dir if absent.
func writeFlockMCP(path string, servers map[string]mcpServerEntry) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("# MCP servers available to this project's routines. Managed by `cass flock wire`\n")
	b.WriteString("# — edit by hand too, but the whole file is rewritten on the next wire.\n")
	b.WriteString("# Each routine wires a subset via its routine.toml `mcp = [...]` selector (omit = all).\n")
	b.WriteString("# Bearer tokens are referenced by ENV-VAR NAME only — never put a secret here.\n")
	b.WriteString("# The Cassandra fleet authenticates uniformly via $CASS_MCP_KEY (cass writes it to\n")
	b.WriteString("# ~/.cass/env on login; flock-server loads it).\n\n")

	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)

	for i, name := range names {
		entry := servers[name]
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "[servers.%s]\n", name)
		if entry.URL != "" {
			fmt.Fprintf(&b, "url = %s\n", tomlQuote(entry.URL))
		}
		if entry.BearerEnv != "" {
			fmt.Fprintf(&b, "bearer_env = %s\n", tomlQuote(entry.BearerEnv))
		}
		if entry.Command != "" {
			fmt.Fprintf(&b, "command = %s\n", tomlQuote(entry.Command))
		}
		if len(entry.Args) > 0 {
			fmt.Fprintf(&b, "args = %s\n", tomlStringArray(entry.Args))
		}
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// ─── tiny TOML scalar helpers ────────────────────────────────────────────────

func tomlQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

func tomlUnquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
		s = strings.ReplaceAll(s, `\"`, `"`)
		s = strings.ReplaceAll(s, `\\`, `\`)
		return s
	}
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		return s[1 : len(s)-1]
	}
	return s
}

func tomlStringArray(items []string) string {
	parts := make([]string, len(items))
	for i, it := range items {
		parts[i] = tomlQuote(it)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// parseTOMLStringArray parses an inline ["a", "b"] array. Best-effort: handles
// the simple single-line quoted-string array flock emits for stdio args.
func parseTOMLStringArray(s string) []string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		out = append(out, tomlUnquote(part))
	}
	return out
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
