# cass

> Go CLI for the Cassandra platform — auth, MCP keys, cookies, and client setup.

`cass` is the developer entrypoint to the Cassandra access plane. It authenticates a
user against the portal (Google OAuth), provisions and refreshes per-service MCP keys,
syncs browser cookies into the auth service, and wires those keys into Claude Code and
Codex so the MCP services are reachable. The service catalog it installs from is the
same set of cassandra-* MCP services that sit behind cassandra-kit's ACL enforcer.

## Architecture

Cobra + Charm CLI, single binary, built from `cmd/cass`. Layout:

- `internal/cmd/` — one file per command (Cobra). `root.go` wires everything; `Version`
  is injected via ldflags.
- `internal/registry/` — the in-source service catalog. Each entry points at the GitHub
  repo where that service's `cass-manifest.json` lives. `Name` must exactly match the
  FastMCP server's `SERVICE_ID` and the portal's `MCP_SERVICES.id` (`/keys/validate` is
  an exact-match lookup). Kept in source so `cass setup` works on a clean machine.
- `internal/manifest/` — fetches/parses service manifests.
- Support packages: `internal/auth`, `internal/portal`, `internal/config`,
  `internal/browser`, `internal/share`, `internal/claudecfg`, `internal/shellrc`,
  `internal/tui`.

Deploy target: a standalone Go binary, cross-compiled and shipped as GitHub releases
(Homebrew tap + Scoop bucket manifests live under `homebrew/` and `scoop/`). Not a k8s
or ArgoCD workload.

## Quickstart

```bash
# Build for the current host (version stamped from git describe → bin/cass)
scripts/build.sh

# Quick checks
go build ./...
go test ./...
```

Install a released build (macOS / Linux / WSL):

```bash
gh api repos/Cassandras-Edge/cass/contents/install.sh --jq '.content' | base64 -d | sh
```

Windows (native) via Scoop:

```powershell
scoop bucket add cassandra https://github.com/Cassandras-Edge/cass
scoop install cass
```

Set up:

```bash
cass login                   # browser, Google OAuth via the portal
cass whoami                  # verify identity
cass setup --client both     # fetch manifests, mint per-service MCP keys, and
                             # write mcpServers entries for Claude Code + Codex
```

`cass setup` is idempotent — re-running it refreshes manifests and rotates keys.
By default it also adds a managed `claude='cass claude'` / `codex='cass codex'`
alias block to `~/.zshrc`, so `claude`/`codex` route through the wrapper (loads
`.cass.toml` defaults + background MCP-key refresh).

## Configuration

| Var | Required | Purpose |
|-----|----------|---------|
| `CASS_PORTAL_URL` | No | Portal base URL (default `https://portal.cassandrasedge.com`) |
| `AUTH_URL` | No | Auth-service base URL (default `https://auth.cassandrasedge.com`) |
| `AUTH_SECRET` | No | Shared admin secret for direct auth-service writes (admin-only commands) |
| `CASS_SHARE_URL` | No | Share-service base URL (default `https://share.cassandrasedge.com`) |
| `TWITTER_MCP_URL` | No | twitter-mcp base URL for `cass twitter sync-queryids` (default `https://twitter-mcp.cassandrasedge.com`) |
| `CASS_GMAIL_CLIENT_ID` / `CASS_GMAIL_CLIENT_SECRET` | No | Gmail OAuth client for `cass gmail link`; baked at build time or supplied at runtime |
| `CASS_INSTALL_DIR` | No | install.sh target dir (default `~/.local/bin`) |
| `CASS_DEV_EMAIL` | No | Dev override for the authenticated identity |
| `CODEX_HOME` | No | Codex config dir used when wiring Codex MCP servers |

## Deployment

Releases are cut with `scripts/release.sh vX.Y.Z` (cross-compiles all platforms, tags,
and creates a GitHub release; `--dry` builds without tagging — requires a clean tree and
authed `gh`). `.github/workflows/release.yml` runs the GitHub-side release on tag push.
`cass update` auto-detects scoop-managed installs and delegates to `scoop update cass`.

## Links

- `internal/registry/registry.go` — the canonical list of services `cass` installs.
- Related: cassandra-kit (auth/ACL/registry), cassandra-portal (key management + OAuth),
  and the cassandra-* MCP services referenced in the registry.
