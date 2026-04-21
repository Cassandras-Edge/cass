"""Schwab OAuth bootstrap via schwab-py and the Cassandra portal."""

from __future__ import annotations

import json
import sys
import tempfile
from pathlib import Path

import click
import httpx
from schwab.auth import client_from_login_flow, client_from_manual_flow

from cass.auth import ensure_auth
from cass.config import get_portal_url

# States where the user needs to run `cass auth schwab` interactively.
# `degraded` is transient (backend retrying) — leave it alone.
_REAUTH_STATES = {"disabled", "reauth_required"}


def _portal_headers() -> dict[str, str]:
    auth = ensure_auth()
    headers: dict[str, str] = {
        "Authorization": f"Bearer {auth['key']}",
        "Content-Type": "application/json",
    }
    if auth.get("cf_token"):
        headers["Cookie"] = f"CF_Authorization={auth['cf_token']}"
    return headers


def _load_token(path: Path) -> dict:
    return json.loads(path.read_text())


@click.group()
def auth() -> None:
    """Authenticate against upstream services (Schwab, …)."""


@auth.command("schwab")
@click.option("--session-id", default="", help="Existing portal connect session id.")
@click.option("--manual", is_flag=True, help="Use schwab-py's manual flow instead of opening a local browser.")
def auth_schwab(session_id: str, manual: bool) -> None:
    """Authenticate Schwab using schwab-py's normal OAuth flow and upload the token."""
    portal = get_portal_url()
    headers = _portal_headers()

    with httpx.Client(timeout=60.0, headers=headers) as client:
        if not session_id:
            bootstrap = client.post(f"{portal}/api/schwab/bootstrap")
            bootstrap.raise_for_status()
            data = bootstrap.json()
            session_id = data["session_id"]
        else:
            bootstrap = client.post(f"{portal}/api/schwab/bootstrap?session_id={session_id}")
            bootstrap.raise_for_status()
            data = bootstrap.json()

        callback_url = data["callback_url"]
        app_key = data["app_key"]
        app_secret = data["app_secret"]

        click.echo(f"Schwab connect session: {session_id}")
        click.echo(f"Callback URL: {callback_url}")
        click.echo("Starting schwab-py OAuth flow...")

        with tempfile.TemporaryDirectory(prefix="cass-schwab-token-") as tmpdir:
            token_path = Path(tmpdir) / "token.json"
            if manual:
                client_from_manual_flow(
                    api_key=app_key,
                    app_secret=app_secret,
                    callback_url=callback_url,
                    token_path=str(token_path),
                    asyncio=False,
                    enforce_enums=True,
                )
            else:
                client_from_login_flow(
                    api_key=app_key,
                    app_secret=app_secret,
                    callback_url=callback_url,
                    token_path=str(token_path),
                    asyncio=False,
                    enforce_enums=True,
                    callback_timeout=300.0,
                    interactive=True,
                )
            token = _load_token(token_path)

        complete = client.post(
            f"{portal}/api/schwab/connect/complete/{session_id}",
            json={"token": token},
        )
        complete.raise_for_status()

        session_status = client.get(f"{portal}/api/schwab/status")
        session_status.raise_for_status()
        status_data = session_status.json()
        click.echo(f"Current state: {status_data['state']}")
        click.echo(status_data["message"])


def _fetch_schwab_status() -> dict | None:
    """Return the broker's session status dict, or None if the portal is unreachable."""
    portal = get_portal_url()
    try:
        headers = _portal_headers()
    except Exception:  # noqa: BLE001 — no creds cached, treat as unconfigured
        return None
    try:
        with httpx.Client(timeout=10.0, headers=headers) as client:
            resp = client.get(f"{portal}/api/schwab/status")
            if resp.status_code >= 400:
                return None
            return resp.json()
    except httpx.HTTPError:
        return None


@auth.command("status")
@click.option("--service", "services", multiple=True, help="Limit to specific services (default: all).")
@click.option("--if-needed", is_flag=True, help="Run the re-auth flow inline when a service is not healthy.")
@click.option("--quiet", is_flag=True, help="No output on healthy sessions — only print on problems.")
def auth_status(services: tuple[str, ...], if_needed: bool, quiet: bool) -> None:
    """Check upstream-service auth state. Exits non-zero if anything needs attention.

    Designed for a Claude Code SessionStart hook:

      \b
      { "hooks": { "SessionStart": [{ "hooks": [
          { "type": "command",
            "command": "cass auth status --if-needed --quiet" }
      ] }] } }
    """
    selected = {s.lower() for s in services} if services else None

    if selected is None or "schwab" in selected:
        status = _fetch_schwab_status()
        if status is None:
            if not quiet:
                click.echo("schwab: unknown (portal unreachable or not logged in)")
            sys.exit(1)
        state = status.get("state", "unknown")
        message = status.get("message", "")
        if state == "healthy":
            if not quiet:
                click.echo(f"schwab: healthy — {message}")
        elif state in _REAUTH_STATES:
            click.echo(f"schwab: {state} — {message}", err=True)
            if if_needed:
                click.echo("Running `cass auth schwab`…", err=True)
                ctx = click.get_current_context()
                ctx.invoke(auth_schwab, session_id="", manual=False)
                return
            sys.exit(1)
        else:
            # degraded / refresh_due / unknown — warn but don't force re-auth
            click.echo(f"schwab: {state} — {message}", err=True)
            sys.exit(1)
