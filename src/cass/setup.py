"""Setup Claude Code and Codex with Cassandra integrations."""

from __future__ import annotations

import json
import shlex
import shutil
import subprocess
from pathlib import Path

import os

import click
import httpx

from cass.patched_cli import _install_prebuilt
from cass.refresh_keys import PLUGIN_SERVICES, _load_settings, _save_settings, _write_plugin_option


CODEX_ENV_PATH = Path.home() / ".config" / "cass" / "codex-mcp.env"


MARKETPLACE_REPO = "Cassandras-Edge/cassandra-marketplace"
MARKETPLACE_NAME = "cassandra-plugins"

# Plugins installed by default on `cass setup`. Kept intentionally narrow —
# the everyday "what people are saying / market data" set plus the
# share-convo skill so teleporting sessions works out of the box.
# Everything else is opt-in via `--with`.
DEFAULT_PLUGINS = [
    "media-mcp", "twitter-mcp", "reddit-mcp", "discord-mcp", "market-research",
    "share-convo",
]

# Opt-in plugins. Install via `cass setup --with <name>` or `--with all`.
OPTIONAL_PLUGINS = [
    "stopgate",          # session rate-limit guard
    "claudeai-mcp",      # read/write your claude.ai account
    "gemini-mcp",        # grounded web search via Gemini
    "perplexity-mcp",    # grounded web search via Perplexity
    "routines-mcp",      # fire/inspect your autonomous routines
    "cass-image",        # skill teaching Claude to use `cass image` via Bash
    "tradingview-mcp",   # owner-only ACL (NekoKeys Pro account)
    "schwab-mcp",        # per-user Schwab OAuth — run `cass auth schwab` after
]

ALL_PLUGINS = DEFAULT_PLUGINS + OPTIONAL_PLUGINS

CODEX_SERVERS: dict[str, dict[str, str]] = {
    "yt-mcp": {"service": "yt-mcp", "subdomain": "youtube"},
    "discord-mcp": {"service": "discord-mcp", "subdomain": "discord-mcp"},
    "twitter-mcp": {"service": "twitter-mcp", "subdomain": "twitter-mcp"},
    "market-research": {"service": "market-research", "subdomain": "market-research"},
    "reddit-mcp": {"service": "reddit-mcp", "subdomain": "reddit"},
    "claudeai-mcp": {"service": "claudeai-mcp", "subdomain": "claude-ai"},
    "gemini-mcp": {"service": "gemini-mcp", "subdomain": "gemini"},
    "perplexity-mcp": {"service": "perplexity-mcp", "subdomain": "perplexity"},
    "gateway": {"service": "gateway", "subdomain": "gateway"},
    "tradingview-mcp": {"service": "tradingview-mcp", "subdomain": "tradingview-mcp"},
    "routines": {"service": "routines", "subdomain": "routines-mcp"},
    "schwab-mcp": {"service": "schwab-mcp", "subdomain": "schwab"},
}
# Mirror the Claude default/optional split. `yt-mcp` is the Codex-side name for
# what Claude calls media-mcp.
DEFAULT_CODEX_SERVERS = [
    "yt-mcp", "discord-mcp", "twitter-mcp", "market-research", "reddit-mcp",
]
OPTIONAL_CODEX_SERVERS = [
    "claudeai-mcp", "gemini-mcp", "perplexity-mcp", "gateway", "routines",
    "tradingview-mcp", "schwab-mcp",
]


def _resolve_opt_ins(
    includes: tuple[str, ...],
    optional_pool: list[str],
    known_pool: list[str] | None = None,
) -> set[str]:
    """Expand --with values into a concrete set of optional plugins to enable.

    `optional_pool` is the set applicable to this client; only names in it get
    selected. `known_pool` (defaults to `optional_pool`) is the set we validate
    against — pass a union across clients so a Claude-only name doesn't error
    when resolving for Codex. Names valid-somewhere-but-not-here are silently
    skipped.
    """
    if known_pool is None:
        known_pool = optional_pool
    selected: set[str] = set()
    for raw in includes:
        for piece in raw.split(","):
            name = piece.strip()
            if not name:
                continue
            if name == "all":
                selected.update(optional_pool)
                continue
            if name not in known_pool:
                raise click.ClickException(
                    f"Unknown optional plugin '{name}'. Known: {', '.join(known_pool)}, or 'all'."
                )
            if name in optional_pool:
                selected.add(name)
    return selected


def _run_claude(*args: str) -> bool:
    """Run a claude CLI command. Returns True on success."""
    claude = shutil.which("claude")
    if not claude:
        raise click.ClickException("claude CLI not found in PATH. Install Claude Code first.")
    result = subprocess.run([claude, *args], capture_output=True, text=True, timeout=30)
    if result.returncode != 0:
        stderr = result.stderr.strip()
        if stderr:
            click.echo(f"  warning: {stderr}", err=True)
        return False
    return True


def _available_clients() -> list[str]:
    clients: list[str] = []
    if shutil.which("claude"):
        clients.append("claude")
    if shutil.which("codex"):
        clients.append("codex")
    return clients


def _resolve_clients(client: str) -> list[str]:
    if client == "both":
        clients = ["claude", "codex"]
    elif client == "auto":
        clients = _available_clients()
    else:
        clients = [client]

    if not clients:
        raise click.ClickException("Neither claude nor codex CLI found in PATH.")

    missing = [name for name in clients if shutil.which(name) is None]
    if missing:
        raise click.ClickException(f"Missing required CLI(s): {', '.join(missing)}")

    return clients


def sync_platform(
    install_missing: bool,
    client: str = "auto",
    opt_in_claude: set[str] | None = None,
    opt_in_codex: set[str] | None = None,
    scope: str = "project",
) -> None:
    """Refresh Cassandra integrations for Claude Code, Codex, or both."""
    clients = _resolve_clients(client)

    if "claude" in clients:
        _sync_claude(install_missing, opt_in_claude or set(), scope=scope)

    if "codex" in clients:
        if "claude" in clients:
            click.echo("")
        _sync_codex(install_missing, opt_in_codex or set())


def _sync_claude(install_missing: bool, opt_in: set[str], scope: str = "project") -> None:
    click.echo(f"Refreshing Claude marketplace (scope: {scope})...")
    _run_claude("plugin", "marketplace", "update", "cassandra-plugins")

    click.echo("")
    click.echo("Updating patched Claude CLI...")
    try:
        _install_prebuilt(None)
    except click.ClickException as e:
        click.echo(f"  warning: {e.message}", err=True)
    except Exception as e:
        click.echo(f"  warning: patched-cli install failed: {e}", err=True)

    installed = _read_installed_plugins_by_scope()
    touched: list[str] = []
    skipped_optional: list[str] = []
    for plugin in ALL_PLUGINS:
        qualified = f"{plugin}@cassandra-plugins"
        is_optional = plugin in OPTIONAL_PLUGINS
        installed_scope = installed.get(qualified)

        if installed_scope:
            click.echo(f"Updating {plugin} (scope: {installed_scope})...")
            _run_claude("plugin", "update", qualified, "--scope", installed_scope)
            touched.append(plugin)
        elif is_optional and plugin not in opt_in:
            skipped_optional.append(plugin)
        elif install_missing:
            click.echo(f"Enabling {plugin} (scope: {scope})...")
            _run_claude("plugin", "install", qualified, "--scope", scope)
            touched.append(plugin)

    if skipped_optional:
        click.echo("")
        click.echo(
            "Skipped optional: " + ", ".join(skipped_optional)
            + " — enable with `cass setup --with <name>` (or `--with all`)."
        )

    if not touched:
        click.echo("")
        click.echo("No Cassandra Claude plugins installed. Run `cass setup --client claude` to enable them.")
        return

    click.echo("")
    click.echo("Populating Claude MCP keys...")
    try:
        _populate_mcp_keys(touched)
    except click.ClickException as e:
        click.echo(f"  warning: {e.message}", err=True)
        click.echo("  Run `cass refresh-keys` manually to retry.", err=True)


def _codex_has_server(name: str) -> bool:
    codex = shutil.which("codex")
    if not codex:
        return False
    result = subprocess.run(
        [codex, "mcp", "get", name, "--json"],
        capture_output=True,
        text=True,
        timeout=15,
    )
    return result.returncode == 0


def _codex_env_var(service: str) -> str:
    normalized = service.upper().replace("-", "_")
    return f"CASS_MCP_{normalized}_KEY"


def _codex_url(subdomain: str) -> str:
    return f"https://{subdomain}.cassandrasedge.com/mcp"


def _load_codex_env() -> dict[str, str]:
    if not CODEX_ENV_PATH.exists():
        return {}
    vals: dict[str, str] = {}
    for line in CODEX_ENV_PATH.read_text().splitlines():
        stripped = line.strip()
        if not stripped or stripped.startswith("#"):
            continue
        if stripped.startswith("export "):
            stripped = stripped[len("export "):]
        if "=" not in stripped:
            continue
        key, value = stripped.split("=", 1)
        vals[key.strip()] = value.strip().strip("'").strip('"')
    return vals


def _save_codex_env(values: dict[str, str]) -> None:
    merged = _load_codex_env()
    merged.update(values)
    CODEX_ENV_PATH.parent.mkdir(parents=True, exist_ok=True)
    lines = [
        "# Generated by `cass setup --client codex`.",
        "# Source this before launching Codex so bearer-token env vars are available.",
        "",
    ]
    for key in sorted(merged):
        lines.append(f"export {key}={shlex.quote(merged[key])}")
    CODEX_ENV_PATH.write_text("\n".join(lines) + "\n")
    CODEX_ENV_PATH.chmod(0o600)


def _upsert_codex_server(name: str, url: str, env_var: str) -> None:
    codex = shutil.which("codex")
    if not codex:
        raise click.ClickException("codex CLI not found in PATH. Install Codex first.")
    if _codex_has_server(name):
        subprocess.run([codex, "mcp", "remove", name], check=True, timeout=15)
    subprocess.run(
        [codex, "mcp", "add", name, "--url", url, "--bearer-token-env-var", env_var],
        check=True,
        timeout=30,
    )


def _sync_codex(install_missing: bool, opt_in: set[str]) -> None:
    click.echo("Syncing Codex MCP servers...")

    import os  # noqa: PLC0415
    universal_key = os.environ.get("CASS_MCP_KEY", "")
    if not universal_key:
        raise click.ClickException(
            "CASS_MCP_KEY not set in env. Run `cass login`, then "
            "`source ~/.cass/env`, then re-run setup."
        )

    touched: list[str] = []
    skipped_optional: list[str] = []
    env_updates: dict[str, str] = {}

    # All servers share one bearer token — the per-device CASS_MCP_KEY.
    # We still register each server with its own env-var name (CASS_MCP_KEY)
    # so codex can resolve it consistently across servers.
    UNIVERSAL_ENV_VAR = "CASS_MCP_KEY"

    for name, meta in CODEX_SERVERS.items():
        exists = _codex_has_server(name)
        is_optional = name in OPTIONAL_CODEX_SERVERS
        if not exists and is_optional and name not in opt_in:
            skipped_optional.append(name)
            continue
        if not exists and not install_missing:
            continue

        click.echo(f"Configuring {name} → {meta['subdomain']}...")
        env_updates[UNIVERSAL_ENV_VAR] = universal_key
        _upsert_codex_server(name, _codex_url(meta["subdomain"]), UNIVERSAL_ENV_VAR)
        touched.append(name)

    if skipped_optional:
        click.echo("")
        click.echo(
            "Skipped optional: " + ", ".join(skipped_optional)
            + " — enable with `cass setup --client codex --with <name>`."
        )

    if not touched:
        click.echo("")
        click.echo("No Cassandra Codex MCP servers configured. Run `cass setup --client codex` to add them.")
        return

    _save_codex_env(env_updates)

    click.echo("")
    click.echo(f"Configured {len(touched)} Codex MCP server(s).")
    click.echo(f"Bearer tokens written to: {CODEX_ENV_PATH}")
    click.echo("Source that file before launching Codex so the MCP auth env vars are available.")


def _ensure_device_authorized(device_name: str | None, force_reauth: bool) -> None:
    """Run the device login ceremony if creds are missing/expired/near-expiry.

    Idempotent: with valid creds and not --force, this is a no-op.
    Otherwise drives the browser-loopback flow (cass.auth._run_login_flow),
    which writes ~/.cass/env and returns.

    When env file is sourced after this returns, CASS_MCP_KEY etc. are
    in os.environ and downstream setup steps see them.
    """
    import datetime as _dt  # noqa: PLC0415
    import socket  # noqa: PLC0415
    from cass.cli_auth import ENV_PATH, get_env_credentials  # noqa: PLC0415
    from cass.config import get_portal_url  # noqa: PLC0415

    # Source the env file if it exists but isn't in our os.environ yet
    # (common case: setup invoked from a shell that started before env was
    # written or hasn't sourced ~/.cass/env).
    creds = get_env_credentials()
    if not creds and ENV_PATH.exists():
        for line in ENV_PATH.read_text().splitlines():
            line = line.strip()
            if not line or line.startswith("#"):
                continue
            if line.startswith("export "):
                line = line[len("export "):]
            if "=" not in line:
                continue
            k, v = line.split("=", 1)
            os.environ[k] = v.strip().strip("'").strip('"')
        creds = get_env_credentials()

    expiry_warn_days = 7
    needs_login = force_reauth or creds is None
    expiring = False

    if creds and not force_reauth:
        # Probe portal for the key's expires_at. If missing/expired, re-auth.
        try:
            r = httpx.get(
                f"{get_portal_url()}/api/extension/whoami",
                headers={
                    "CF-Access-Client-Id": creds["cf_access_client_id"],
                    "CF-Access-Client-Secret": creds["cf_access_client_secret"],
                    "Authorization": f"Bearer {creds['mcp_key']}",
                },
                timeout=8,
            )
            if r.status_code == 401:
                click.echo("  existing creds rejected (revoked/expired) — re-authorizing")
                needs_login = True
            elif r.status_code == 200:
                exp = (r.json() or {}).get("expires_at")
                if exp:
                    expires = _dt.datetime.fromisoformat(exp.replace("Z", "+00:00"))
                    if expires.tzinfo is None:
                        expires = expires.replace(tzinfo=_dt.timezone.utc)
                    delta = expires - _dt.datetime.now(_dt.timezone.utc)
                    if delta.total_seconds() <= 0:
                        click.echo("  device creds expired — re-authorizing")
                        needs_login = True
                    elif delta.days <= expiry_warn_days:
                        click.echo(
                            f"  device creds expire in {delta.days} day(s) — "
                            "re-authorizing to refresh",
                        )
                        needs_login = True
                        expiring = True
        except httpx.HTTPError:
            click.echo("  could not reach portal to validate creds — proceeding "
                       "with whatever's in env", err=True)

    if not needs_login:
        click.echo(f"  device creds present and healthy (email: {creds['email']})")
        return

    if not device_name:
        default = socket.gethostname().split(".")[0]
        if click.get_text_stream("stdin").isatty():
            device_name = click.prompt("Device name", default=default)
        else:
            device_name = default

    click.echo(f"  authorizing device '{device_name}'...")
    from cass.auth import _run_login_flow  # noqa: PLC0415
    _run_login_flow(device_name=device_name)

    # Re-source env after login so the rest of setup sees the new creds.
    if ENV_PATH.exists():
        for line in ENV_PATH.read_text().splitlines():
            line = line.strip()
            if line.startswith("export "):
                line = line[len("export "):]
            if not line or line.startswith("#") or "=" not in line:
                continue
            k, v = line.split("=", 1)
            os.environ[k] = v.strip().strip("'").strip('"')


def _run_setup_for_clients(
    client: str, includes: tuple[str, ...] = (), scope: str = "project",
    device_name: str | None = None, force_reauth: bool = False,
) -> None:
    """Shared setup flow for one or more client integrations."""
    clients = _resolve_clients(client)

    # 1. Make sure this device is authorized (creds present + not expiring).
    click.echo("Checking device authorization...")
    _ensure_device_authorized(device_name, force_reauth)
    click.echo("")

    all_optional = sorted(set(OPTIONAL_PLUGINS) | set(OPTIONAL_CODEX_SERVERS))
    opt_in_claude = _resolve_opt_ins(includes, OPTIONAL_PLUGINS, all_optional)
    opt_in_codex = _resolve_opt_ins(includes, OPTIONAL_CODEX_SERVERS, all_optional)

    if "claude" in clients:
        click.echo("Adding Cassandra marketplace...")
        _run_claude("plugin", "marketplace", "add", MARKETPLACE_REPO)
        # `claude plugin marketplace add` only writes user scope; mirror into
        # project/local settings so plugins installed there can resolve it.
        if scope in ("project", "local"):
            _ensure_marketplace_in_scope_settings(scope)
        if "codex" in clients:
            click.echo("")

    sync_platform(
        install_missing=True,
        client=client,
        opt_in_claude=opt_in_claude,
        opt_in_codex=opt_in_codex,
        scope=scope,
    )

    click.echo("")
    if "claude" in clients:
        click.echo("Claude plugins (default):")
        for p in DEFAULT_PLUGINS:
            click.echo(f"  - {p}")
        if OPTIONAL_PLUGINS:
            click.echo("Optional (opt in with --with <name>):")
            for p in OPTIONAL_PLUGINS:
                marker = "✓" if p in opt_in_claude else " "
                click.echo(f"  {marker} {p}")
        click.echo("")
        click.echo("Restart Claude Code to activate plugins.")
    if "codex" in clients:
        if "claude" in clients:
            click.echo("")
        click.echo("Restart Codex after sourcing the generated env file to activate the MCP servers.")


_SCOPE_CHOICES = ["user", "project", "local"]


@click.command()
@click.option(
    "--client",
    type=click.Choice(["auto", "claude", "codex", "both"]),
    default="auto",
    show_default=True,
    help="Which client integrations to set up.",
)
@click.option(
    "--scope",
    type=click.Choice(_SCOPE_CHOICES),
    default="project",
    show_default=True,
    help=(
        "Claude plugin install scope. 'project' keeps plugins scoped to the "
        "current repo (committed via .claude/), 'local' is per-checkout, "
        "'user' is global. (Codex MCP is always global; this flag is ignored "
        "for Codex.)"
    ),
)
@click.option(
    "--with",
    "includes",
    multiple=True,
    metavar="NAME",
    help=(
        "Enable an optional plugin by name (repeatable, or comma-separated). "
        "Use `all` to enable every optional plugin. "
        "Currently optional: " + ", ".join(OPTIONAL_PLUGINS) + "."
    ),
)
@click.option(
    "--device", "device_name", default=None,
    help="Device name to register (default: prompt with hostname).",
)
@click.option(
    "--reauth", is_flag=True,
    help="Force a fresh device login even if existing creds are valid. "
         "Use after a leak suspicion or to manually rotate.",
)
def setup(client: str, scope: str, includes: tuple[str, ...],
          device_name: str | None, reauth: bool) -> None:
    """First-time Cassandra setup for Claude Code, Codex, or both.

    Idempotent + interactive. Steps:
      1. Check device authorization. Missing or near-expiry → run the
         browser-loopback flow (prompts for device name first).
      2. Register the Cassandra plugin marketplace.
      3. Install / update plugins (default + any --with selections).
      4. Write per-device mcp_key into all plugin user_configs.

    Re-running `cass setup` is the canonical way to refresh credentials —
    the per-device CF token + mcp_key both expire after 90 days; setup
    auto-detects and re-auths.
    """
    _run_setup_for_clients(client, includes, scope=scope,
                           device_name=device_name, force_reauth=reauth)


@click.group()
def codex() -> None:
    """Codex-specific Cassandra commands."""


@codex.command("setup")
@click.option(
    "--with", "includes", multiple=True, metavar="NAME",
    help="Enable an optional server by name (repeatable, or comma-separated). "
         "Use `all` for every optional server.",
)
def codex_setup(includes: tuple[str, ...]) -> None:
    """Set up Codex MCP servers and auth for Cassandra services."""
    _run_setup_for_clients("codex", includes)


@click.group()
def claude() -> None:
    """Claude-specific Cassandra commands."""


@claude.command("setup")
@click.option(
    "--scope",
    type=click.Choice(_SCOPE_CHOICES),
    default="project",
    show_default=True,
    help="Plugin install scope.",
)
def claude_setup(scope: str) -> None:
    """Set up the Cassandra Claude marketplace plugins."""
    _run_setup_for_clients("claude", scope=scope)


def _ensure_marketplace_in_scope_settings(scope: str) -> None:
    """Merge extraKnownMarketplaces into the project/local settings file.

    Claude Code's `plugin marketplace add` always writes user scope; for
    project/local scopes we edit the settings file directly so the marketplace
    is self-contained at the same scope as enabledPlugins.
    """
    if scope == "project":
        path = Path.cwd() / ".claude" / "settings.json"
    elif scope == "local":
        path = Path.cwd() / ".claude" / "settings.local.json"
    else:
        return

    data: dict = {}
    if path.exists():
        try:
            data = json.loads(path.read_text())
            if not isinstance(data, dict):
                data = {}
        except json.JSONDecodeError:
            data = {}

    marketplaces = data.setdefault("extraKnownMarketplaces", {})
    entry = {"source": {"source": "github", "repo": MARKETPLACE_REPO}}
    if marketplaces.get(MARKETPLACE_NAME) == entry:
        return

    marketplaces[MARKETPLACE_NAME] = entry
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(data, indent=2) + "\n")
    click.echo(f"  wrote marketplace to {path}")


def _read_installed_plugins(scope: str = "project") -> set[str]:
    """Return plugin IDs (name@marketplace) already installed in the given scope."""
    return {pid for pid, s in _read_installed_plugins_by_scope().items() if s == scope}


def _read_installed_plugins_by_scope() -> dict[str, str]:
    """Return {plugin_id: scope} for every installed plugin across all scopes.

    Uses `claude plugin list --json` so we see the authoritative current state
    rather than guessing from settings files. Lets the caller update each plugin
    at whichever scope it lives in, rather than assuming all share one scope.
    """
    claude = shutil.which("claude")
    if not claude:
        return {}
    try:
        result = subprocess.run(
            [claude, "plugin", "list", "--json"],
            capture_output=True, text=True, timeout=15, check=False,
        )
        if result.returncode != 0:
            return {}
        entries = json.loads(result.stdout)
    except (json.JSONDecodeError, subprocess.TimeoutExpired, OSError):
        return {}
    return {e["id"]: e["scope"] for e in entries if "id" in e and "scope" in e}


def _populate_mcp_keys(plugins: list[str]) -> None:
    """Write the user's per-device mcp_key into each plugin's user_config.

    Post-redesign: keys are per-DEVICE, not per-service. `cass login` writes
    a single mcp_key to ~/.cass/env (as CASS_MCP_KEY); we copy that same
    value into all plugin manifests' ${user_config.mcpKey}.
    """
    import os  # noqa: PLC0415
    needs_keys = [p for p in plugins if p in PLUGIN_SERVICES]
    if not needs_keys:
        return

    universal_key = os.environ.get("CASS_MCP_KEY", "")
    if not universal_key:
        click.echo(
            "  warning: CASS_MCP_KEY not set in env — run `cass login` to mint a "
            "per-device key, then re-run setup.",
            err=True,
        )
        return

    settings = _load_settings()
    for plugin in needs_keys:
        _write_plugin_option(settings, plugin, "mcpKey", universal_key)
    _save_settings(settings)
    click.echo(f"  wrote per-device mcp_key to {len(needs_keys)} plugin(s)")


# ---------- teardown (inverse of setup) ----------


def _teardown_claude(scope: str) -> list[str]:
    """Uninstall all Cassandra Claude plugins at the given scope."""
    installed = _read_installed_plugins(scope=scope)
    removed: list[str] = []
    for plugin in ALL_PLUGINS:
        qualified = f"{plugin}@cassandra-plugins"
        if qualified not in installed:
            continue
        click.echo(f"Removing {plugin} (scope: {scope})...")
        if _run_claude("plugin", "uninstall", qualified, "--scope", scope):
            removed.append(plugin)
    return removed


def _teardown_codex() -> list[str]:
    """Remove all Cassandra Codex MCP servers (always global — no scope)."""
    codex = shutil.which("codex")
    if not codex:
        raise click.ClickException("codex CLI not found in PATH.")
    removed: list[str] = []
    for name in CODEX_SERVERS:
        if not _codex_has_server(name):
            continue
        click.echo(f"Removing {name}...")
        try:
            subprocess.run([codex, "mcp", "remove", name], check=True, timeout=15)
            removed.append(name)
        except subprocess.CalledProcessError as e:
            click.echo(f"  warning: failed to remove {name}: {e}", err=True)
    return removed


def _run_teardown_for_clients(client: str, scope: str, assume_yes: bool) -> None:
    """Shared teardown flow for one or more client integrations."""
    clients = _resolve_clients(client)

    if not assume_yes:
        targets = []
        if "claude" in clients:
            targets.append(f"Claude plugins (scope: {scope})")
        if "codex" in clients:
            targets.append("Codex MCP servers (global)")
        click.echo("This will remove: " + ", ".join(targets) + ".")
        click.echo("Marketplace registration + generated env files are kept.")
        click.confirm("Proceed?", default=False, abort=True)

    removed_claude: list[str] = []
    removed_codex: list[str] = []

    if "claude" in clients:
        removed_claude = _teardown_claude(scope)
        if "codex" in clients:
            click.echo("")

    if "codex" in clients:
        removed_codex = _teardown_codex()

    click.echo("")
    if "claude" in clients:
        click.echo(f"Claude: removed {len(removed_claude)} plugin(s)"
                   + (f" — {', '.join(removed_claude)}" if removed_claude else ""))
    if "codex" in clients:
        click.echo(f"Codex: removed {len(removed_codex)} MCP server(s)"
                   + (f" — {', '.join(removed_codex)}" if removed_codex else ""))


@click.command()
@click.option(
    "--client",
    type=click.Choice(["auto", "claude", "codex", "both"]),
    default="auto",
    show_default=True,
    help="Which client integrations to tear down.",
)
@click.option(
    "--scope",
    type=click.Choice(_SCOPE_CHOICES),
    default="project",
    show_default=True,
    help="Claude plugin scope to remove from. (Ignored for Codex.)",
)
@click.option("--yes", "-y", "assume_yes", is_flag=True,
              help="Skip the confirmation prompt.")
def teardown(client: str, scope: str, assume_yes: bool) -> None:
    """Remove Cassandra plugins / MCP servers (inverse of `cass setup`).

    Keeps the marketplace registration and generated env files so you can
    re-run `cass setup` cleanly. Does not uninstall `cass` itself.
    """
    _run_teardown_for_clients(client, scope, assume_yes)


@codex.command("teardown")
@click.option("--yes", "-y", "assume_yes", is_flag=True,
              help="Skip the confirmation prompt.")
def codex_teardown(assume_yes: bool) -> None:
    """Remove Cassandra Codex MCP servers."""
    _run_teardown_for_clients("codex", scope="project", assume_yes=assume_yes)


@claude.command("teardown")
@click.option("--scope", type=click.Choice(_SCOPE_CHOICES),
              default="project", show_default=True,
              help="Plugin scope to remove from.")
@click.option("--yes", "-y", "assume_yes", is_flag=True,
              help="Skip the confirmation prompt.")
def claude_teardown(scope: str, assume_yes: bool) -> None:
    """Remove Cassandra Claude plugins."""
    _run_teardown_for_clients("claude", scope=scope, assume_yes=assume_yes)
