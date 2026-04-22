"""Setup Claude Code and Codex with Cassandra integrations."""

from __future__ import annotations

import json
import shlex
import shutil
import subprocess
from pathlib import Path

import click

from cass.patched_cli import _install_prebuilt, require_supported_host
from cass.refresh_keys import PLUGIN_SERVICES, _fetch_new_key, _load_settings, _save_service_key, _save_settings, _write_plugin_option, get_service_key


INSTALLED_PLUGINS_PATH = Path.home() / ".claude" / "plugins" / "installed_plugins.json"
CODEX_ENV_PATH = Path.home() / ".config" / "cass" / "codex-mcp.env"


MARKETPLACE_REPO = "Cassandras-Edge/cassandra-marketplace"

# Plugins installed by default on `cass setup`. Safe for any user — no
# per-user credentials, no owner-gated ACLs, useful broadly.
DEFAULT_PLUGINS = [
    "stopgate", "media-mcp", "twitter-mcp", "reddit-mcp", "claudeai-mcp",
    "discord-mcp", "market-research", "gemini-mcp", "perplexity-mcp",
    "routines-mcp", "cass-image",
]

# Opt-in plugins. Each has a specific reason to be off by default
# (owner-only ACL, per-user brokerage auth, etc.). Install via
# `cass setup --with <name>` or `cass setup --with all`.
OPTIONAL_PLUGINS = [
    "tradingview-mcp",  # owner-only ACL (NekoKeys Pro account)
    "schwab-mcp",       # per-user Schwab OAuth — run `cass auth schwab` after
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
# Same default/optional split applies to Codex servers.
DEFAULT_CODEX_SERVERS = [
    "yt-mcp", "discord-mcp", "twitter-mcp", "market-research", "reddit-mcp",
    "claudeai-mcp", "gemini-mcp", "perplexity-mcp", "gateway", "routines",
]
OPTIONAL_CODEX_SERVERS = ["tradingview-mcp", "schwab-mcp"]


def _resolve_opt_ins(includes: tuple[str, ...], optional_pool: list[str]) -> set[str]:
    """Expand --with values into a concrete set of optional plugins to enable."""
    selected: set[str] = set()
    for raw in includes:
        for piece in raw.split(","):
            name = piece.strip()
            if not name:
                continue
            if name == "all":
                selected.update(optional_pool)
                continue
            if name not in optional_pool:
                raise click.ClickException(
                    f"Unknown optional plugin '{name}'. Known: {', '.join(optional_pool)}, or 'all'."
                )
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
) -> None:
    """Refresh Cassandra integrations for Claude Code, Codex, or both."""
    clients = _resolve_clients(client)

    if "claude" in clients:
        _sync_claude(install_missing, opt_in_claude or set())

    if "codex" in clients:
        if "claude" in clients:
            click.echo("")
        _sync_codex(install_missing, opt_in_codex or set())


def _sync_claude(install_missing: bool, opt_in: set[str]) -> None:
    require_supported_host()

    click.echo("Refreshing Claude marketplace...")
    _run_claude("plugin", "marketplace", "update", "cassandra-plugins")

    click.echo("")
    click.echo("Updating patched Claude CLI...")
    try:
        _install_prebuilt(None)
    except click.ClickException as e:
        click.echo(f"  warning: {e.message}", err=True)
    except Exception as e:
        click.echo(f"  warning: patched-cli install failed: {e}", err=True)

    installed = _read_installed_plugins()
    touched: list[str] = []
    skipped_optional: list[str] = []
    for plugin in ALL_PLUGINS:
        qualified = f"{plugin}@cassandra-plugins"
        is_optional = plugin in OPTIONAL_PLUGINS
        already_installed = qualified in installed

        if already_installed:
            # Keep what the user already chose up to date, always.
            click.echo(f"Updating {plugin}...")
            _run_claude("plugin", "update", qualified)
            touched.append(plugin)
        elif is_optional and plugin not in opt_in:
            skipped_optional.append(plugin)
        elif install_missing:
            click.echo(f"Enabling {plugin}...")
            _run_claude("plugin", "install", qualified)
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

    from cass.auth import ensure_auth  # noqa: PLC0415

    touched: list[str] = []
    skipped_optional: list[str] = []
    env_updates: dict[str, str] = {}
    auth: dict | None = None

    for name, meta in CODEX_SERVERS.items():
        exists = _codex_has_server(name)
        is_optional = name in OPTIONAL_CODEX_SERVERS
        if not exists and is_optional and name not in opt_in:
            skipped_optional.append(name)
            continue
        if not exists and not install_missing:
            continue

        if auth is None:
            auth = ensure_auth()

        service = meta["service"]
        key = get_service_key(service)
        if not key:
            click.echo(f"Creating key for {service}...")
            key = _fetch_new_key(service, auth)
            _save_service_key(service, key, auth.get("email", ""))
        else:
            click.echo(f"Using cached key for {service}...")

        env_var = _codex_env_var(service)
        env_updates[env_var] = key
        _upsert_codex_server(name, _codex_url(meta["subdomain"]), env_var)
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


def _run_setup_for_clients(client: str, includes: tuple[str, ...] = ()) -> None:
    """Shared setup flow for one or more client integrations."""
    clients = _resolve_clients(client)

    opt_in_claude = _resolve_opt_ins(includes, OPTIONAL_PLUGINS)
    opt_in_codex = _resolve_opt_ins(includes, OPTIONAL_CODEX_SERVERS)

    if "claude" in clients:
        click.echo("Adding Cassandra marketplace...")
        _run_claude("plugin", "marketplace", "add", MARKETPLACE_REPO)
        if "codex" in clients:
            click.echo("")

    sync_platform(
        install_missing=True,
        client=client,
        opt_in_claude=opt_in_claude,
        opt_in_codex=opt_in_codex,
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


@click.command()
@click.option(
    "--client",
    type=click.Choice(["auto", "claude", "codex", "both"]),
    default="auto",
    show_default=True,
    help="Which client integrations to set up.",
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
def setup(client: str, includes: tuple[str, ...]) -> None:
    """First-time Cassandra setup for Claude Code, Codex, or both.

    `cass setup` is idempotent. On Claude it registers the marketplace and
    enables Cassandra plugins. On Codex it provisions MCP keys, writes a local
    env file, and registers the Cassandra MCP servers with `codex mcp add`.

    Default-installed plugins: every safe-for-anyone service. Optional plugins
    are opt-in (e.g. `schwab-mcp` needs per-user Schwab auth). Already-
    installed optional plugins are always kept up to date.
    """
    _run_setup_for_clients(client, includes)


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
def claude_setup() -> None:
    """Set up the Cassandra Claude marketplace plugins."""
    _run_setup_for_clients("claude")


def _read_installed_plugins() -> set[str]:
    if not INSTALLED_PLUGINS_PATH.exists():
        return set()
    try:
        data = json.loads(INSTALLED_PLUGINS_PATH.read_text())
        return set(data.get("plugins", {}).keys())
    except json.JSONDecodeError:
        return set()


def _populate_mcp_keys(plugins: list[str]) -> None:
    from cass.auth import ensure_auth  # noqa: PLC0415 — avoid import cycle on `cass --version`
    import httpx  # noqa: PLC0415
    needs_keys = [p for p in plugins if p in PLUGIN_SERVICES]
    if not needs_keys:
        return
    auth = ensure_auth()
    settings = _load_settings()
    for plugin in needs_keys:
        service = PLUGIN_SERVICES[plugin]
        key = get_service_key(service)
        if not key:
            try:
                click.echo(f"  creating key for {service}...")
                key = _fetch_new_key(service, auth)
                _save_service_key(service, key, auth.get("email", ""))
            except httpx.HTTPStatusError as e:
                click.echo(f"  warning: could not provision {service}: {e.response.status_code}", err=True)
                continue
            except Exception as e:  # noqa: BLE001
                click.echo(f"  warning: could not provision {service}: {e}", err=True)
                continue
        else:
            click.echo(f"  using cached key for {service}")
        _write_plugin_option(settings, plugin, "mcpKey", key)
    _save_settings(settings)
