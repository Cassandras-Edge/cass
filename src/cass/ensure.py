"""Ensure a valid MCP key exists for a service, creating one if needed."""

from __future__ import annotations

import json
from pathlib import Path

import click
import httpx

from cass.auth import get_cached_auth
from cass.config import get_portal_url

KEYS_DIR = Path.home() / ".config" / "cass" / "keys"


def _key_path(service: str) -> Path:
    return KEYS_DIR / f"{service}.json"


def get_service_key(service: str) -> str | None:
    """Return cached MCP key for a service, or None."""
    path = _key_path(service)
    if not path.exists():
        return None
    try:
        data = json.loads(path.read_text())
        return data.get("key")
    except (json.JSONDecodeError, KeyError):
        return None


def _save_service_key(service: str, key: str, email: str) -> None:
    KEYS_DIR.mkdir(parents=True, exist_ok=True)
    path = _key_path(service)
    path.write_text(json.dumps({"key": key, "service": service, "email": email}, indent=2))
    path.chmod(0o600)


@click.command("ensure-key")
@click.argument("service")
@click.option("--quiet", "-q", is_flag=True, help="Only output the key, no status messages.")
@click.option("--header", "-H", is_flag=True, help="Output as JSON headers for headersHelper.")
def ensure_key(service: str, quiet: bool, header: bool) -> None:
    """Ensure an MCP key exists for SERVICE. Creates one if needed.

    With --header, outputs JSON headers for Claude Code headersHelper:
      {"Authorization": "Bearer mcp_..."}
    With --quiet, outputs just the key.
    """
    # Check cache first
    existing = get_service_key(service)
    if existing:
        if header:
            click.echo(json.dumps({"Authorization": f"Bearer {existing}"}))
        elif quiet:
            click.echo(existing)
        else:
            click.echo(f"Key for {service}: {existing[:20]}...")
        return

    # Need to create — check login
    auth = get_cached_auth()
    if not auth:
        raise click.ClickException("Not logged in. Run: cass login")

    portal = get_portal_url()
    headers = {
        "Authorization": f"Bearer {auth['key']}",
        "Content-Type": "application/json",
    }

    # Validate our login key is still good
    if not quiet:
        click.echo(f"Creating key for {service}...")

    try:
        # Use the portal's extension whoami to verify auth + get email
        resp = httpx.get(
            f"{portal}/api/extension/whoami",
            headers=headers,
            timeout=15,
        )
        if resp.status_code == 401:
            raise click.ClickException("Login expired. Run: cass login")
        resp.raise_for_status()
        email = resp.json().get("email", auth.get("email", ""))
    except httpx.ConnectError:
        # Portal not reachable — try to use the auth from login directly
        email = auth.get("email", "")
        if not email:
            raise click.ClickException("Portal unreachable and no cached email. Run: cass login")

    # Create key via portal API
    # First, find user's default project
    try:
        resp = httpx.get(
            f"{portal}/api/projects",
            headers=headers,
            timeout=15,
        )
        resp.raise_for_status()
        projects = resp.json()
        project_id = projects[0]["id"] if projects else "default"
    except Exception:
        project_id = "default"

    # Create the key
    try:
        resp = httpx.post(
            f"{portal}/api/projects/{project_id}/services/{service}/keys",
            headers=headers,
            json={"name": f"cass-cli-{service}"},
            timeout=15,
        )
        resp.raise_for_status()
        data = resp.json()
        key = data["key"]
    except httpx.HTTPStatusError as e:
        raise click.ClickException(f"Failed to create key: {e.response.status_code} {e.response.text}") from e

    _save_service_key(service, key, email)

    if header:
        click.echo(json.dumps({"Authorization": f"Bearer {key}"}))
    elif quiet:
        click.echo(key)
    else:
        click.echo(f"Created key for {service}: {key[:20]}...")
