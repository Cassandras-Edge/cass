"""Schwab OAuth bootstrap via schwab-py and the Cassandra portal."""

from __future__ import annotations

import json
import tempfile
from pathlib import Path

import click
import httpx
from schwab.auth import client_from_login_flow, client_from_manual_flow

from cass.auth import ensure_auth
from cass.config import get_portal_url


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
def schwab() -> None:
    """Manage Schwab authentication and session recovery."""


@schwab.command()
@click.option("--session-id", default="", help="Existing portal connect session id.")
@click.option("--manual", is_flag=True, help="Use schwab-py's manual flow instead of opening a local browser.")
def auth(session_id: str, manual: bool) -> None:
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
