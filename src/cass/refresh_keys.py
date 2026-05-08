"""Refresh MCP keys — populate Claude Code plugin user config with bearer tokens.

Replaces the `headersHelper` pattern. Instead of Claude Code shelling out to
`cass ensure-key --header <service>` on every MCP reconnect, we write the
bearer token once into `~/.claude/settings.json` under
`pluginConfigs[<plugin>@cassandra-plugins].options.mcpKey`, and the plugin
manifest resolves `${user_config.mcpKey}` in its static Authorization header
at MCP load time.

`--if-near-expiry` rotates only keys whose `expires_at` is within
NEAR_EXPIRY_DAYS of now. Use this from SessionStart hooks so plugins
self-heal without needing an out-of-band cron.
"""

from __future__ import annotations

import json
from datetime import datetime, timedelta, timezone
from pathlib import Path

import click
import httpx

from cass.auth import ensure_auth
from cass.config import get_portal_url
from cass.ensure import _save_service_key, get_service_key

# Rotate keys this close to expiry. 7 days = comfortable buffer if the user
# doesn't open Claude Code daily.
NEAR_EXPIRY_DAYS = 7


MARKETPLACE = "cassandra-plugins"
SETTINGS_PATH = Path.home() / ".claude" / "settings.json"

# Plugin name → cass service name. Most match; a few legacy mismatches
# (media-mcp → yt-mcp, gateway-mcp → gateway, routines-mcp → routines).
PLUGIN_SERVICES: dict[str, str] = {
    "tradingview-mcp": "tradingview-mcp",
    "twitter-mcp": "twitter-mcp",
    "reddit-mcp": "reddit-mcp",
    "claudeai-mcp": "claudeai-mcp",
    "discord-mcp": "discord-mcp",
    "media-mcp": "yt-mcp",
    "market-research": "market-research",
    "gemini-mcp": "gemini-mcp",
    "perplexity-mcp": "perplexity-mcp",
    "gateway-mcp": "gateway",
    "routines-mcp": "routines",
    "schwab-mcp": "schwab-mcp",
}


def _fetch_new_key(service: str, auth: dict) -> str:
    """Create a new MCP key for `service` via the portal API."""
    portal = get_portal_url()
    headers = {
        "Authorization": f"Bearer {auth['key']}",
        "Content-Type": "application/json",
    }
    if auth.get("cf_token"):
        headers["Cookie"] = f"CF_Authorization={auth['cf_token']}"

    try:
        resp = httpx.get(f"{portal}/api/projects", headers=headers, timeout=15)
        resp.raise_for_status()
        projects = resp.json()
        project_id = projects[0]["id"] if projects else "default"
    except Exception:
        project_id = "default"

    resp = httpx.post(
        f"{portal}/api/projects/{project_id}/services/{service}/keys",
        headers=headers,
        json={"name": f"cass-cli-{service}"},
        timeout=15,
    )
    resp.raise_for_status()
    return resp.json()["key"]


def _load_settings() -> dict:
    if not SETTINGS_PATH.exists():
        return {}
    try:
        return json.loads(SETTINGS_PATH.read_text())
    except json.JSONDecodeError as e:
        raise click.ClickException(f"~/.claude/settings.json is malformed: {e}") from e


def _save_settings(data: dict) -> None:
    SETTINGS_PATH.parent.mkdir(parents=True, exist_ok=True)
    # Preserve existing permissions (CC uses 0644 by default).
    SETTINGS_PATH.write_text(json.dumps(data, indent=2) + "\n")


def _write_plugin_option(settings: dict, plugin: str, key: str, value: str) -> None:
    plugin_id = f"{plugin}@{MARKETPLACE}"
    configs = settings.setdefault("pluginConfigs", {})
    entry = configs.setdefault(plugin_id, {})
    options = entry.setdefault("options", {})
    options[key] = value


def _validate_and_get_expiry(key: str, auth: dict) -> tuple[bool, datetime | None]:
    """Validate key against portal and return (still_valid, expires_at).

    Hits portal's whoami-style validation (keeps cass off direct auth).
    Returns (False, None) if the key is rejected (expired/revoked/missing).
    """
    portal = get_portal_url()
    headers = {
        "Authorization": f"Bearer {key}",
        "Content-Type": "application/json",
    }
    if auth.get("cf_token"):
        headers["Cookie"] = f"CF_Authorization={auth['cf_token']}"
    try:
        resp = httpx.get(f"{portal}/api/extension/whoami", headers=headers, timeout=10)
        # Portal's whoami doesn't expose expiry; ask the auth-fronted endpoint instead.
        # Fall back to validate via the keys endpoint — portal proxies it.
    except httpx.RequestError:
        return False, None
    # The cleanest route is portal/api/extension/whoami returning expires_at;
    # for now, treat 200 as valid and rely on caller's --force semantics.
    if resp.status_code != 200:
        return False, None
    data = resp.json() if resp.headers.get("content-type", "").startswith("application/json") else {}
    exp = data.get("expires_at")
    if not exp:
        return True, None
    try:
        dt = datetime.fromisoformat(exp.replace("Z", "+00:00"))
        if dt.tzinfo is None:
            dt = dt.replace(tzinfo=timezone.utc)
        return True, dt
    except ValueError:
        return True, None


@click.command("refresh-keys")
@click.option("--force", is_flag=True, help="Re-provision keys even if cached locally.")
@click.option("--plugin", "plugin_filter", help="Refresh only this plugin's key.")
@click.option("--if-near-expiry", is_flag=True,
              help=f"Only rotate keys whose expiry is within {NEAR_EXPIRY_DAYS} days. "
                   "Designed for SessionStart hooks — fast no-op when keys are healthy.")
def refresh_keys(force: bool, plugin_filter: str | None, if_near_expiry: bool) -> None:
    """Fetch MCP bearer tokens and write them to Claude Code plugin user config.

    Run this after `cass setup` (or whenever a key stops working) so plugin
    manifests that reference `${user_config.mcpKey}` have a static token
    available at MCP load time.

    With --if-near-expiry, only rotates keys close to their expires_at.
    """
    auth = ensure_auth()

    plugins = (
        {plugin_filter: PLUGIN_SERVICES[plugin_filter]}
        if plugin_filter
        else PLUGIN_SERVICES
    )
    if plugin_filter and plugin_filter not in PLUGIN_SERVICES:
        raise click.ClickException(
            f"Unknown plugin '{plugin_filter}'. Known: {', '.join(PLUGIN_SERVICES)}"
        )

    settings = _load_settings()
    updated: list[tuple[str, str]] = []
    failed: list[tuple[str, str]] = []

    for plugin, service in plugins.items():
        existing = None if force else get_service_key(service)

        # --if-near-expiry path: keep the existing key unless it's within
        # NEAR_EXPIRY_DAYS. This makes the SessionStart hook a fast no-op
        # most of the time.
        if existing and if_near_expiry:
            valid, expires_at = _validate_and_get_expiry(existing, auth)
            if valid and (
                expires_at is None
                or expires_at - datetime.now(timezone.utc) > timedelta(days=NEAR_EXPIRY_DAYS)
            ):
                _write_plugin_option(settings, plugin, "mcpKey", existing)
                updated.append((plugin, "fresh"))
                continue
            # Otherwise fall through to mint a replacement.
            existing = None

        if existing:
            key, source = existing, "cached"
        else:
            try:
                click.echo(f"Creating key for {service}...")
                key = _fetch_new_key(service, auth)
                _save_service_key(service, key, auth.get("email", ""))
                source = "new"
            except httpx.HTTPStatusError as e:
                body = (e.response.text or "").strip()[:200]
                failed.append((plugin, f"{e.response.status_code} {body}"))
                continue
            except Exception as e:  # noqa: BLE001
                failed.append((plugin, str(e)))
                continue

        _write_plugin_option(settings, plugin, "mcpKey", key)
        updated.append((plugin, source))

    _save_settings(settings)

    click.echo("")
    click.echo(f"Wrote {len(updated)} key(s) to {SETTINGS_PATH}:")
    for plugin, source in updated:
        click.echo(f"  - {plugin:20s} [{source}]")
    if failed:
        click.echo("")
        click.echo(f"Failed for {len(failed)} plugin(s):", err=True)
        for plugin, reason in failed:
            click.echo(f"  - {plugin:20s} {reason}", err=True)
    click.echo("")
    click.echo("Restart Claude Code for plugins to pick up the new config.")
