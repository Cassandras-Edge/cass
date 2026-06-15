package cmd

import (
	"github.com/spf13/cobra"

	"github.com/Cassandras-Edge/cass/internal/tui"
)

// Version is stamped at build time via:
//
//	go build -ldflags="-X github.com/Cassandras-Edge/cass/internal/cmd.Version=v0.7.4"
//
// `scripts/build.sh` does this automatically from `git describe --tags`.
var Version = "dev"

func New() *cobra.Command {
	root := &cobra.Command{
		Use:     "cass",
		Short:   "Cassandra platform CLI",
		Long:    "Auth, keys, account links, and AI-client management for the Cassandra platform.",
		Version: Version,
		// No subcommand → drop into the interactive dashboard.
		RunE: func(_ *cobra.Command, _ []string) error {
			return tui.Run()
		},
		SilenceUsage: true,
	}

	tui.SetLinkAccounts(tuiLinkAccounts())

	// The CLI does four jobs; each is a command namespace. A fifth group
	// (tools) holds device-local utilities. Old flat invocations survive
	// via hidden top-level aliases registered at the bottom.
	root.AddCommand(authCmd())
	root.AddCommand(keysGroupCmd())
	root.AddCommand(linkCmd())
	root.AddCommand(clientCmd())
	root.AddCommand(toolsCmd())
	root.AddCommand(newMcpCmd())

	registerAliases(root)
	return root
}

// authCmd — job 1: who you are + which devices may act as you.
//
// The pre-reorg `auth` group only held schwab + status; auth is now the real
// identity parent. The schwab linking command moved to `link schwab`;
// `auth status` stays here.
func authCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Identity & device authorization",
	}
	cmd.AddCommand(loginCmd())
	cmd.AddCommand(logoutCmd())
	cmd.AddCommand(whoamiCmd())
	cmd.AddCommand(devicesCmd())
	cmd.AddCommand(authStatusCmd())
	return cmd
}

// keysGroupCmd — job 2: the per-service MCP key cache + rotation.
//
// keysCmd() already provides list + clear. ensure / refresh are folded in
// from their standalone constructors; we only rename their .Use verbs here
// (ensure-key → ensure, refresh-keys → refresh) without touching the
// constructor files. The old verbs stay reachable via top-level aliases.
func keysGroupCmd() *cobra.Command {
	cmd := keysCmd()

	ec := ensureKeyCmd()
	ec.Use = "ensure <service>"
	cmd.AddCommand(ec)

	rc := refreshKeysCmd()
	rc.Use = "refresh"
	cmd.AddCommand(rc)

	return cmd
}

// linkAccount is the single source of truth for an external-account link: its
// cobra command, optional top-level alias, the badge-probe service id, and how
// it appears in the interactive dashboard. Add one row here and it shows up in
// the `cass link` CLI group, the top-level aliases, AND the TUI menu — no
// second edit in the tui package.
type linkAccount struct {
	new       func() *cobra.Command
	alias     string   // top-level alias; "" = none (e.g. schwab is link-only)
	menuLabel string   // dashboard row label; "" = hidden from the menu
	menuArgs  []string // command path the dashboard execs
	svc       string   // badge probe service id; "" = no badge
}

var linkAccounts = []linkAccount{
	{gmailCmd, "gmail", "gmail", []string{"link", "gmail", "link"}, "gmail-mcp"},
	{plaudCmd, "plaud", "plaud", []string{"link", "plaud", "login"}, "plaud-mcp"},
	{discordCmd, "discord", "discord", []string{"link", "discord", "login"}, "discord-mcp"},
	{twitterCmd, "twitter", "twitter", []string{"link", "twitter", "sync-queryids"}, "twitter-mcp"},
	{youtubeCmd, "youtube", "youtube", []string{"youtube", "link"}, "yt-mcp"},
	{tradingViewCmd, "tradingview", "tradingview", []string{"link", "tradingview", "setup"}, "tradingview-mcp"},
	{authSchwabCmd, "", "schwab", []string{"link", "schwab"}, "schwab-mcp"},
	{cookiesCmd, "cookies", "cookies", []string{"link", "cookies", "sync"}, ""},
}

// linkCmd — job 3: link external accounts so the fleet can act on them.
// Every Cassandra service authenticates with an mcp_key bearer; these
// subcommands provision the per-account upstream credentials (cookies,
// OAuth tokens, query IDs) the services need behind that bearer.
func linkCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "link",
		Short: "Link external accounts (Gmail, Plaud, Discord, Twitter, YouTube, TradingView, Schwab, cookies)",
	}
	for _, la := range linkAccounts {
		cmd.AddCommand(la.new())
	}
	return cmd
}

// tuiLinkAccounts projects the link-account table into the dashboard menu rows.
func tuiLinkAccounts() []tui.LinkAccount {
	var out []tui.LinkAccount
	for _, la := range linkAccounts {
		if la.menuLabel == "" {
			continue
		}
		out = append(out, tui.LinkAccount{Label: la.menuLabel, Args: la.menuArgs, Svc: la.svc})
	}
	return out
}

// clientCmd — job 4: wire local AI clients (Claude Code, Codex, flock) to
// the fleet and run them. setup/config manage the wiring; claude/codex/flock
// are passthrough wrappers that exec the real CLI with cass defaults.
func clientCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "client",
		Short: "Configure & run AI clients (Claude Code, Codex, flock)",
	}
	cmd.AddCommand(setupCmd())
	cmd.AddCommand(addCmd())
	cmd.AddCommand(claudeCmd())
	cmd.AddCommand(codexCmd())
	cmd.AddCommand(flockCmd())
	cmd.AddCommand(configCmd())
	return cmd
}

// toolsCmd — device-local utilities: image gen, session sharing, self-update
// / install, and teardown.
func toolsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tools",
		Short: "Device utilities (image, share, update, install, teardown)",
	}
	cmd.AddCommand(imageCmd())
	cmd.AddCommand(shareCmd())
	cmd.AddCommand(updateCmd())
	cmd.AddCommand(installCmd())
	cmd.AddCommand(teardownCmd())
	return cmd
}

// registerAliases wires every historical top-level invocation back onto the
// root as a hidden command (see alias() in aliases.go). --help stays clean
// because the canonical homes are the five groups above; these only exist so
// muscle memory and existing scripts/hooks never break.
func registerAliases(root *cobra.Command) {
	// auth group — note: no `auth` alias. The real authCmd() group already
	// owns the `auth` name on root (a second AddCommand("auth") would shadow
	// it). Back-compat for the old `auth schwab` is preserved via the new
	// `link schwab`; `auth status` still lives under the real auth group.
	root.AddCommand(alias("login", loginCmd))
	root.AddCommand(alias("logout", logoutCmd))
	root.AddCommand(alias("whoami", whoamiCmd))
	root.AddCommand(alias("devices", devicesCmd))

	// keys group — note: no `keys` alias, same reason as `auth` above. The real
	// keysGroupCmd() owns the `keys` name and is a strict superset
	// (list/clear/ensure/refresh); the legacy flat verbs are preserved below.
	root.AddCommand(alias("ensure-key", ensureKeyCmd))
	root.AddCommand(alias("refresh-keys", refreshKeysCmd))

	// link group — driven by the linkAccounts table (schwab has no alias).
	for _, la := range linkAccounts {
		if la.alias != "" {
			root.AddCommand(alias(la.alias, la.new))
		}
	}

	// client group
	root.AddCommand(alias("setup", setupCmd))
	root.AddCommand(alias("add", addCmd))
	root.AddCommand(alias("claude", claudeCmd))
	root.AddCommand(alias("codex", codexCmd))
	root.AddCommand(alias("flock", flockCmd))
	root.AddCommand(alias("config", configCmd))

	// tools group
	root.AddCommand(alias("image", imageCmd))
	root.AddCommand(alias("share", shareCmd))
	root.AddCommand(alias("update", updateCmd))
	root.AddCommand(alias("install", installCmd))
	root.AddCommand(alias("teardown", teardownCmd))
}
