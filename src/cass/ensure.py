"""Ensure a valid MCP key exists for a service, creating one if needed."""

from __future__ import annotations

import json
from pathlib import Path

import click
import httpx

from cass.auth import ensure_auth, get_cached_auth
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


def _key_is_alive(key: str) -> bool:
    """Probe portal→auth to confirm a cached key still exists in auth's DB.

    Cached keys can go stale when the auth service loses the row (PVC reset,
    portal→auth write previously failed silently, manual deletion, etc.).
    Re-serving a dead key sends Claude Code's headersHelper a token the MCP
    server will reject with invalid_token, so we validate before returning.

    Returns True only on an authoritative "valid". On any error (portal down,
    network blip, auth unreachable) we return True so we don't thrash new
    keys during transient failures — the MCP server itself is the final gate.
    """
    portal = get_portal_url()
    auth = get_cached_auth()
    headers: dict[str, str] = {"Content-Type": "application/json"}
    if auth:
        headers["Authorization"] = f"Bearer {auth['key']}"
        if auth.get("cf_token"):
            headers["Cookie"] = f"CF_Authorization={auth['cf_token']}"
    try:
        resp = httpx.post(
            f"{portal}/api/keys/validate",
            headers=headers,
            json={"key": key},
            timeout=5,
        )
    except httpx.HTTPError:
        return True  # don't thrash on transient network failures
    if resp.status_code != 200:
        return True  # portal error — let the MCP server decide
    try:
        return bool(resp.json().get("valid"))
    except (ValueError, KeyError):
        return True


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
    # Check cache first — but validate against the auth service before
    # handing it back. A stale cached key would otherwise cause every MCP
    # connection to fail with invalid_token forever.
    existing = get_service_key(service)
    if existing:
        if _key_is_alive(existing):
            if header:
                click.echo(json.dumps({"Authorization": f"Bearer {existing}"}))
            elif quiet:
                click.echo(existing)
            else:
                click.echo(f"Key for {service}: {existing[:20]}...")
            return
        # Stale cache — delete and fall through to re-provision.
        _key_path(service).unlink(missing_ok=True)
        if not quiet:
            click.echo(f"Cached key for {service} is no longer valid — re-provisioning...", err=True)

    # Need to create — ensure valid auth (auto-triggers browser login if CF Access expired)
    auth = ensure_auth()

    portal = get_portal_url()
    headers: dict[str, str] = {
        "Authorization": f"Bearer {auth['key']}",
        "Content-Type": "application/json",
    }
    if auth.get("cf_token"):
        headers["Cookie"] = f"CF_Authorization={auth['cf_token']}"

    if not quiet:
        click.echo(f"Creating key for {service}...")

    email = auth.get("email", "")

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
