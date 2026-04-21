# cass

Cassandra platform CLI — auth, keys, cookies, and service management.

## Install

```bash
# With gh CLI (works with private repos)
gh release download --repo Cassandras-Edge/cass --pattern 'cass-darwin-arm64' --dir ~/.local/bin
mv ~/.local/bin/cass-darwin-arm64 ~/.local/bin/cass && chmod +x ~/.local/bin/cass

# Or use the install script (auto-detects platform)
gh api repos/Cassandras-Edge/cass/contents/install.sh --jq '.content' | base64 -d | sh
```

Installs to `~/.local/bin/cass`. Set `CASS_INSTALL_DIR` to change the location.

Make sure `~/.local/bin` is in your PATH.

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
| `cass update` | Update to the latest version |

## Auto-update

`cass` checks for updates on every run (at most once per hour). Set `CASS_NO_AUTO_UPDATE=1` to disable.

## Claude Code + Codex

`cass` now supports both client flows:

- Claude Code: `cass claude setup` registers the Cassandra marketplace, installs plugins, and populates plugin MCP keys.
- Codex: `cass codex setup` provisions Cassandra MCP keys, registers the remote MCP servers with `codex mcp add`, and writes bearer-token exports to `~/.config/cass/codex-mcp.env`.

For Codex, source the generated env file before launching Codex so the configured `bearer_token_env_var` entries resolve correctly.
