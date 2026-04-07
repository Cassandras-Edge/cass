"""MCP key management via auth service API."""

from __future__ import annotations

import json
import secrets

import click
import httpx

from cass.config import get_default_email, require_auth


def _auth_client() -> httpx.Client:
    base_url, headers = require_auth()
    return httpx.Client(base_url=base_url, headers=headers, timeout=15)


@click.group()
def keys() -> None:
    """Manage MCP keys."""


@keys.command()
@click.argument("service")
@click.argument("name")
@click.option("--email", "-e", default=None, help="Creator email.")
@click.option("--project", "-p", default="default", help="Project ID.")
def create(service: str, name: str, email: str | None, project: str) -> None:
    """Create a new MCP key for SERVICE with NAME."""
    email = email or get_default_email()
    key_id = f"mcp_{secrets.token_hex(24)}"

    with _auth_client() as client:
        resp = client.put(
            f"/keys/{key_id}",
            json={
                "service": service,
                "name": name,
                "created_by": email,
                "project_id": project,
            },
        )
        resp.raise_for_status()

    click.echo(f"Created key: {key_id}")
    click.echo(f"  service: {service}")
    click.echo(f"  name: {name}")
    click.echo(f"  email: {email}")


@keys.command()
@click.argument("key_id")
def validate(key_id: str) -> None:
    """Validate an MCP key and show its details."""
    with _auth_client() as client:
        resp = client.post("/keys/validate", json={"key": key_id})
        resp.raise_for_status()
        data = resp.json()

    if not data.get("valid"):
        click.echo("Invalid key.", err=True)
        raise SystemExit(1)

    click.echo(f"Valid: {data.get('valid')}")
    click.echo(f"Email: {data.get('email')}")
    click.echo(f"Service: {data.get('service')}")
    creds = data.get("credentials", {})
    if creds:
        click.echo("Credentials:")
        for k, v in creds.items():
            if isinstance(v, str) and len(v) > 40:
                click.echo(f"  {k}: {v[:20]}...({len(v)} chars)")
            else:
                click.echo(f"  {k}: {v}")


@keys.command()
@click.argument("key_id")
def delete(key_id: str) -> None:
    """Delete an MCP key."""
    with _auth_client() as client:
        resp = client.request("DELETE", f"/keys/{key_id}")
        resp.raise_for_status()
    click.echo(f"Deleted: {key_id}")


@keys.command()
@click.argument("key_id")
@click.argument("creds_json")
def set_credentials(key_id: str, creds_json: str) -> None:
    """Set credentials on an MCP key (JSON string)."""
    try:
        creds = json.loads(creds_json)
    except json.JSONDecodeError as e:
        raise click.ClickException(f"Invalid JSON: {e}") from e

    with _auth_client() as client:
        resp = client.patch(f"/keys/{key_id}/credentials", json={"credentials": creds})
        resp.raise_for_status()
    click.echo(f"Updated credentials on {key_id}")
