"""Setup Claude Code with the Cassandra marketplace and plugins."""

from __future__ import annotations

import json
import shutil
import subprocess
from pathlib import Path

import click

from cass.patched_cli import _install_prebuilt, require_supported_host
from cass.refresh_keys import PLUGIN_SERVICES, _fetch_new_key, _load_settings, _save_service_key, _save_settings, _write_plugin_option, get_service_key


INSTALLED_PLUGINS_PATH = Path.home() / ".claude" / "plugins" / "installed_plugins.json"


MARKETPLACE_REPO = "Cassandras-Edge/cassandra-marketplace"
ALL_PLUGINS = [
    "stopgate", "media-mcp", "twitter-mcp", "reddit-mcp", "claudeai-mcp",
    "discord-mcp", "market-research", "gemini-mcp", "perplexity-mcp",
    "tradingview-mcp",
]


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


def sync_platform(install_missing: bool) -> None:
    """Refresh marketplace + patched CLI + plugins + MCP keys. Shared by
    `cass setup` (install_missing=True — enable every Cassandra plugin) and
    `cass update` (install_missing=False — only touch what's already
    installed, respecting the user's opt-out choices).
    """
    require_supported_host()
    claude = shutil.which("claude")
    if not claude:
        raise click.ClickException("claude CLI not found in PATH. Install Claude Code first.")

    click.echo("Refreshing marketplace...")
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
    for plugin in ALL_PLUGINS:
        qualified = f"{plugin}@cassandra-plugins"
        if qualified in installed:
            click.echo(f"Updating {plugin}...")
            _run_claude("plugin", "update", qualified)
            touched.append(plugin)
        elif install_missing:
            click.echo(f"Enabling {plugin}...")
            _run_claude("plugin", "install", qualified)
            touched.append(plugin)

    if not touched:
        click.echo("")
        click.echo("No Cassandra plugins installed. Run `cass setup` to enable them.")
        return

    click.echo("")
    click.echo("Populating MCP keys...")
    try:
        _populate_mcp_keys(touched)
    except click.ClickException as e:
        click.echo(f"  warning: {e.message}", err=True)
        click.echo("  Run `cass refresh-keys` manually to retry.", err=True)


@click.command()
def setup() -> None:
    """First-time Claude Code setup with the Cassandra marketplace.

    Registers the marketplace and enables every Cassandra plugin. Idempotent —
    re-running is equivalent to `cass update` except it also re-adds the
    marketplace (a no-op if already registered).
    """
    click.echo("Adding Cassandra marketplace...")
    _run_claude("plugin", "marketplace", "add", MARKETPLACE_REPO)

    sync_platform(install_missing=True)

    click.echo("")
    click.echo("Done! Installed plugins:")
    for p in ALL_PLUGINS:
        click.echo(f"  - {p}")
    click.echo("")
    click.echo("Restart Claude Code to activate plugins.")


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
