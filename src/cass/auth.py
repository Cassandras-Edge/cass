"""CLI login — browser OAuth via portal, caches MCP key locally."""

from __future__ import annotations

import base64
import json
import time
import webbrowser
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
from urllib.parse import parse_qs, urlparse

import click

from cass.config import get_portal_url

AUTH_FILE = Path.home() / ".config" / "cass" / "auth.json"


def get_cached_auth() -> dict | None:
    if not AUTH_FILE.exists():
        return None
    try:
        data = json.loads(AUTH_FILE.read_text())
        if data.get("key") and data.get("email"):
            return data
    except (json.JSONDecodeError, KeyError):
        pass
    return None


def _cf_token_valid(token: str) -> bool:
    """Check if a CF Access JWT is still valid (not expired)."""
    try:
        payload_b64 = token.split(".")[1]
        # Fix base64 padding
        padding = 4 - len(payload_b64) % 4
        if padding != 4:
            payload_b64 += "=" * padding
        payload = json.loads(base64.urlsafe_b64decode(payload_b64))
        exp = payload.get("exp", 0)
        # Valid if >5 min remaining
        return time.time() < (exp - 300)
    except Exception:
        return False


def ensure_auth() -> dict:
    """Get valid auth, auto-triggering browser login if needed."""
    auth = get_cached_auth()

    if auth and auth.get("cf_token") and _cf_token_valid(auth["cf_token"]):
        return auth

    # Need fresh auth — trigger browser login
    if auth and auth.get("cf_token"):
        click.echo("CF Access session expired — re-authenticating...")
    elif auth:
        click.echo("No CF Access token cached — authenticating...")
    else:
        click.echo("Not logged in — opening browser to authenticate...")

    _run_login_flow()

    auth = get_cached_auth()
    if not auth:
        raise click.ClickException("Login failed — no credentials received")
    if not auth.get("cf_token"):
        raise click.ClickException("Login succeeded but no CF Access token received. Is portal updated?")
    return auth


def _run_login_flow(device_name: str | None = None) -> None:
    """Open browser for OAuth login and wait for callback.

    Portal mints a per-device CF Access service token + per-device mcp_key
    during this flow. Both are passed back via the localhost callback and
    written to ~/.cass/env (so cass + plugin manifests use them) plus the
    legacy ~/.config/cass/auth.json (CF cookie compat during migration).
    """
    import socket  # noqa: PLC0415
    from cass.cli_auth import (  # noqa: PLC0415 — avoid circular import on cli boot
        ENV_PATH, ensure_zprofile_sources_env, write_env_file,
    )

    result: dict = {}
    name = (device_name or socket.gethostname().split(".")[0])[:64]

    class CallbackHandler(BaseHTTPRequestHandler):
        def do_GET(self) -> None:  # noqa: N802
            parsed = urlparse(self.path)
            params = parse_qs(parsed.query)
            for k in ("key", "email", "cf_token", "device_id", "device_name",
                      "cf_client_id", "cf_client_secret"):
                v = params.get(k, [None])[0]
                if v is not None:
                    result[k] = v

            if result.get("key") and result.get("email"):
                self.send_response(200)
                self.send_header("Content-Type", "text/html")
                self.end_headers()
                self.wfile.write(
                    b"<html><body><h2>Authenticated!</h2>"
                    b"<p>You can close this tab and return to the terminal.</p>"
                    b"<script>window.close()</script></body></html>"
                )
            else:
                self.send_response(400)
                self.send_header("Content-Type", "text/html")
                self.end_headers()
                self.wfile.write(b"<html><body><h2>Login failed</h2></body></html>")

        def log_message(self, format: str, *args: object) -> None:  # noqa: A002
            pass

    server = HTTPServer(("127.0.0.1", 0), CallbackHandler)
    port = server.server_address[1]
    callback_url = f"http://localhost:{port}/callback"
    login_url = (
        f"{get_portal_url()}/api/cli/login"
        f"?callback={callback_url}&device={name}"
    )

    click.echo(f"Opening browser for login (device: {name})...")
    click.echo(f"If it doesn't open, visit: {login_url}")
    webbrowser.open(login_url)

    server.handle_request()
    server.server_close()

    if not result.get("key"):
        raise click.ClickException("Login failed — no key received")

    # Save legacy auth.json for compat with code paths still using cf_token cookie.
    save_auth(result["key"], result["email"], result.get("cf_token"))

    # Write the env file (CF service-token + per-device mcp_key) — this is
    # the path going forward. Portal includes cf_client_id/secret only when
    # PORTAL_ACCESS_APP_ID is configured; if it's missing we skip silently
    # and fall back to the legacy CF cookie path.
    if result.get("cf_client_id") and result.get("cf_client_secret"):
        write_env_file({
            "email": result["email"],
            "device_name": result.get("device_name", name),
            "cf_access_client_id": result["cf_client_id"],
            "cf_access_client_secret": result["cf_client_secret"],
            "mcp_key": result["key"],
        })
        added = ensure_zprofile_sources_env()
        click.echo(f"  Credentials → {ENV_PATH}")
        if added:
            click.echo(f"  Added 'source {ENV_PATH}' to ~/.zprofile")

    click.echo(f"Logged in as {result['email']}")


def save_auth(key: str, email: str, cf_token: str | None = None) -> None:
    AUTH_FILE.parent.mkdir(parents=True, exist_ok=True)
    data: dict = {"key": key, "email": email}
    if cf_token:
        data["cf_token"] = cf_token
    AUTH_FILE.write_text(json.dumps(data, indent=2))
    AUTH_FILE.chmod(0o600)


def clear_auth() -> None:
    if AUTH_FILE.exists():
        AUTH_FILE.unlink()


@click.command()
def login() -> None:
    """Authenticate with the Cassandra portal via browser OAuth."""
    _run_login_flow()
    click.echo(f"Token cached at {AUTH_FILE}")


@click.command()
def logout() -> None:
    """Clear cached authentication."""
    clear_auth()
    click.echo("Logged out — cached token removed.")


@click.command()
def whoami() -> None:
    """Show current authenticated identity."""
    auth = get_cached_auth()
    if not auth:
        click.echo("Not logged in. Run: cass login")
        raise SystemExit(1)
    click.echo(f"Email: {auth['email']}")
    click.echo(f"Key: {auth['key'][:20]}...")
