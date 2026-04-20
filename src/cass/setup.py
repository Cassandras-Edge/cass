"""Setup Claude Code with the Cassandra marketplace and plugins."""

from __future__ import annotations

import shutil
import subprocess

import click

from cass.patched_cli import _install_prebuilt, require_supported_host
from cass.refresh_keys import PLUGIN_SERVICES, _fetch_new_key, _load_settings, _save_service_key, _save_settings, _write_plugin_option, get_service_key


MARKETPLACE_REPO = "Cassandras-Edge/cassandra-marketplace"
DEFAULT_PLUGINS = ["stopgate", "media-mcp", "market-research", "gemini-mcp"]
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


@click.command()
@click.option("--all", "install_all", is_flag=True, help="Enable all MCP plugins, not just defaults.")
def setup(install_all: bool) -> None:
    """Set up Claude Code with the Cassandra marketplace and plugins.

    Registers the marketplace, enables cass-cli and default MCP plugins.
    Use --all to enable every available plugin.
    """
    require_supported_host()
    claude = shutil.which("claude")
    if not claude:
        raise click.ClickException("claude CLI not found in PATH. Install Claude Code first.")

    # Add marketplace
    click.echo("Adding Cassandra marketplace...")
    _run_claude("plugin", "marketplace", "add", MARKETPLACE_REPO)

    # Install the patched CLI at ~/.local/bin/claude-patched — required by
    # stopgate (and any future plugin that needs `claude --bare` + OAuth).
    click.echo("")
    click.echo("Installing patched Claude CLI...")
    try:
        _install_prebuilt(None)
    except click.ClickException as e:
        click.echo(f"  warning: {e.message}", err=True)
        click.echo("  Stopgate hook will silent-fail until `cass patched-cli install` succeeds.", err=True)
    except Exception as e:
        click.echo(f"  warning: patched-cli install failed: {e}", err=True)

    # Pick plugins
    plugins = ALL_PLUGINS if install_all else DEFAULT_PLUGINS

    # Install plugins
    for plugin in plugins:
        qualified = f"{plugin}@cassandra-plugins"
        click.echo(f"Enabling {plugin}...")
        _run_claude("plugin", "install", qualified)

    # Fetch MCP keys and write them to plugin user config. Plugin manifests
    # resolve ${user_config.mcpKey} in static Authorization headers at MCP
    # load time — no per-reconnect shell spawn.
    click.echo("")
    click.echo("Populating MCP keys...")
    try:
        _populate_mcp_keys(plugins)
    except click.ClickException as e:
        click.echo(f"  warning: {e.message}", err=True)
        click.echo("  Run `cass refresh-keys` manually to retry.", err=True)

    click.echo("")
    click.echo("Done! Installed plugins:")
    for p in plugins:
        click.echo(f"  - {p}")

    if not install_all:
        remaining = [p for p in ALL_PLUGINS if p not in plugins]
        if remaining:
            click.echo("")
            click.echo("More plugins available (install with --all or individually):")
            for p in remaining:
                click.echo(f"  - {p}")

    click.echo("")
    click.echo("Restart Claude Code to activate plugins.")


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
