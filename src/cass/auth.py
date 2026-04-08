"""CLI login — browser OAuth via portal, caches MCP key locally."""

from __future__ import annotations

import json
import threading
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
    result: dict = {}

    class CallbackHandler(BaseHTTPRequestHandler):
        def do_GET(self) -> None:  # noqa: N802
            parsed = urlparse(self.path)
            params = parse_qs(parsed.query)

            key = params.get("key", [None])[0]
            email = params.get("email", [None])[0]
            cf_token = params.get("cf_token", [None])[0]

            if key and email:
                result["key"] = key
                result["email"] = email
                if cf_token:
                    result["cf_token"] = cf_token
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
            pass  # suppress request logs

    server = HTTPServer(("127.0.0.1", 0), CallbackHandler)
    port = server.server_address[1]
    callback_url = f"http://localhost:{port}/callback"
    login_url = f"{get_portal_url()}/api/cli/login?callback={callback_url}"

    click.echo(f"Opening browser for login...")
    click.echo(f"If it doesn't open, visit: {login_url}")
    webbrowser.open(login_url)

    # Handle one request (the callback)
    server.handle_request()
    server.server_close()

    if result.get("key"):
        save_auth(result["key"], result["email"], result.get("cf_token"))
        click.echo(f"Logged in as {result['email']}")
        click.echo(f"Token cached at {AUTH_FILE}")
    else:
        raise click.ClickException("Login failed — no key received")


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
