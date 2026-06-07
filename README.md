# cass

Cassandra platform CLI — auth, keys, cookies, and service management.

## Install

### macOS / Linux / WSL

```bash
# With gh CLI (works with private repos)
gh release download --repo Cassandras-Edge/cass --pattern 'cass-darwin-arm64' --dir ~/.local/bin
mv ~/.local/bin/cass-darwin-arm64 ~/.local/bin/cass && chmod +x ~/.local/bin/cass

# Or use the install script (auto-detects platform)
gh api repos/Cassandras-Edge/cass/contents/install.sh --jq '.content' | base64 -d | sh
```

Installs to `~/.local/bin/cass`. Set `CASS_INSTALL_DIR` to change the location.

Make sure `~/.local/bin` is in your PATH.

### Windows (native, via scoop)

```powershell
scoop bucket add cassandra https://github.com/Cassandras-Edge/cass
scoop install cass
```

`cass update` auto-detects scoop-managed installs and delegates to `scoop update cass`, so updates stay in sync with scoop's manifest ledger.

> `cass claude setup` currently still requires WSL for the Claude Code integration. The `cass` binary itself (auth, keys, Codex setup) works on native Windows via scoop.

#### Smart App Control blocks

The `cass.exe` binary is unsigned. Windows 11's Smart App Control (on by default for clean installs) will refuse to run it with `An Application Control policy has blocked this file`. SAC operates below Defender, so `Unblock-File` and `Add-MpPreference -ExclusionPath` don't help — confirmed empirically.

Fix: **Settings → Privacy & security → Windows Security → Smart App Control → Off.** Caveat from Microsoft: re-enabling SAC later requires a Windows reinstall. For technical users running custom CLI tooling this is usually an acceptable trade.

Long-term fix is Authenticode signing in CI (Azure Trusted Signing is ~$10/mo). Revisit if more than one person needs cass on a SAC-enabled machine.

## Setup

```bash
cass login    # opens browser, authenticates via Google OAuth
cass whoami   # verify your identity
cass codex setup   # preferred Codex setup path
cass claude setup  # preferred Claude setup path
```

## Commands

| Command | Description |
|---------|-------------|
| `cass login` | Authenticate with the Cassandra portal (one-time) |
| `cass logout` | Clear cached authentication |
| `cass whoami` | Show current identity |
| `cass ensure-key SERVICE` | Ensure an MCP key exists for a service |
| `cass cookies sync` | Sync YouTube cookies from Firefox to auth service |
| `cass cookies test` | Test yt-dlp cookie extraction |
| `cass keys create SERVICE NAME` | Create a new MCP key |
| `cass keys validate KEY` | Validate an MCP key |
| `cass keys delete KEY` | Delete an MCP key |
| `cass refresh-keys` | Refresh Claude plugin MCP keys in `~/.claude/settings.json` |
| `cass setup` | Set up Claude plugins and/or Codex MCP servers |
| `cass codex setup` | Set up Codex MCP servers and Codex auth env wiring |
| `cass claude setup` | Set up the Cassandra Claude marketplace plugins |
| `cass codex <persona>` | Launch Codex with a named `.cass.toml` persona, if configured |
| `cass update` | Update to the latest version |

## Auto-update

`cass` checks for updates on every run (at most once per hour). Set `CASS_NO_AUTO_UPDATE=1` to disable.

## Claude Code + Codex

`cass` now supports both client flows:

- Claude Code: `cass claude setup` registers the Cassandra marketplace, installs plugins, and populates plugin MCP keys.
- Codex: `cass codex setup` provisions Cassandra MCP keys, registers the remote MCP servers with `codex mcp add`, and writes bearer-token exports to `~/.config/cass/codex-mcp.env`.

For Codex, source the generated env file before launching Codex so the configured `bearer_token_env_var` entries resolve correctly.

## Per-project personas

`cass claude` and `cass codex` are passthrough wrappers. They load the first
`.cass.toml` found from the current directory upward, then prepend configured
args/env before execing the real client.

You can define explicit named personas under a client section:

```toml
[codex]
args = ["--search"]

[codex.personas.finance]
args = ["--profile", "finance"]

[codex.personas.finance.env]
CASS_PERSONA = "finance"
```

Then:

```bash
cass codex finance
cass codex finance "scan today's market setup"
```

Only declared persona names expand. If the first argument is not a configured
persona, `cass codex ...` remains a normal passthrough invocation.
