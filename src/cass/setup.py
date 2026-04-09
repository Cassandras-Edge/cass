"""Setup Claude Code with the Cassandra marketplace and plugins."""

from __future__ import annotations

import shutil
import subprocess

import click


MARKETPLACE_REPO = "Cassandras-Edge/cassandra-marketplace"
DEFAULT_PLUGINS = ["cass-cli", "media-mcp", "market-research", "gemini-mcp"]
ALL_PLUGINS = [
    "cass-cli", "media-mcp", "twitter-mcp", "reddit-mcp", "claudeai-mcp",
    "discord-mcp", "market-research", "gemini-mcp", "perplexity-mcp",
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
    claude = shutil.which("claude")
    if not claude:
        raise click.ClickException("claude CLI not found in PATH. Install Claude Code first.")

    # Add marketplace
    click.echo("Adding Cassandra marketplace...")
    _run_claude("plugin", "marketplace", "add", MARKETPLACE_REPO)

    # Pick plugins
    plugins = ALL_PLUGINS if install_all else DEFAULT_PLUGINS

    # Install plugins
    for plugin in plugins:
        qualified = f"{plugin}@cassandra-plugins"
        click.echo(f"Enabling {plugin}...")
        _run_claude("plugin", "install", qualified)

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
    click.echo("Next: start Claude Code and the plugins will auto-authenticate via cass.")
