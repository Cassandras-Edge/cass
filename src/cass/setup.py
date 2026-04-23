"""Setup Claude Code and Codex with Cassandra integrations."""

from __future__ import annotations

import json
import shlex
import shutil
import subprocess
from pathlib import Path

import click

from cass.patched_cli import _install_prebuilt
from cass.refresh_keys import PLUGIN_SERVICES, _fetch_new_key, _load_settings, _save_service_key, _save_settings, _write_plugin_option, get_service_key


CODEX_ENV_PATH = Path.home() / ".config" / "cass" / "codex-mcp.env"


MARKETPLACE_REPO = "Cassandras-Edge/cassandra-marketplace"
MARKETPLACE_NAME = "cassandra-plugins"

# Plugins installed by default on `cass setup`. Kept intentionally narrow —
# the everyday "what people are saying / market data" set. Everything else is
# opt-in via `--with`.
DEFAULT_PLUGINS = [
    "media-mcp", "twitter-mcp", "reddit-mcp", "discord-mcp", "market-research",
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


def _run_setup_for_clients(
    client: str, includes: tuple[str, ...] = (), scope: str = "project",
) -> None:
    """Shared setup flow for one or more client integrations."""
    clients = _resolve_clients(client)

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
def setup(client: str, scope: str, includes: tuple[str, ...]) -> None:
    """First-time Cassandra setup for Claude Code, Codex, or both.

    `cass setup` is idempotent. On Claude it registers the marketplace and
    enables Cassandra plugins. On Codex it provisions MCP keys, writes a local
    env file, and registers the Cassandra MCP servers with `codex mcp add`.

    Default-installed plugins: every safe-for-anyone service. Optional plugins
    are opt-in (e.g. `schwab-mcp` needs per-user Schwab auth). Already-
    installed optional plugins are always kept up to date.
    """
    _run_setup_for_clients(client, includes, scope=scope)


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
