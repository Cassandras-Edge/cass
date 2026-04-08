"""Discord auth — QR code login flow, runs locally."""

from __future__ import annotations

import asyncio
import io
import json
import sys
from base64 import b64decode, urlsafe_b64encode
from hashlib import sha256

import click
import httpx

from cass.auth import get_cached_auth
from cass.config import get_default_email, get_portal_url, require_auth

REMOTE_AUTH_URL = "wss://remote-auth-gateway.discord.gg/?v=2"
DISCORD_ORIGIN = "https://discord.com"
USER_AGENT = (
    "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) "
    "AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
)


async def _run_qr_login() -> dict | None:
    """Run the Discord QR login flow. Returns {"token": "..."} on success."""
    from cryptography.hazmat.primitives import hashes  # noqa: PLC0415
    from cryptography.hazmat.primitives.asymmetric import padding, rsa  # noqa: PLC0415
    from cryptography.hazmat.primitives.serialization import Encoding, PublicFormat  # noqa: PLC0415
    from websockets import connect as ws_connect  # noqa: PLC0415
    from websockets.typing import Origin  # noqa: PLC0415

    private_key = rsa.generate_private_key(public_exponent=65537, key_size=2048)
    public_key = private_key.public_key()
    public_key_pem = public_key.public_bytes(Encoding.PEM, PublicFormat.SubjectPublicKeyInfo).decode()
    encoded_public_key = "".join(public_key_pem.split("\n")[1:-2])

    def decrypt_payload(encrypted: str) -> bytes:
        return private_key.decrypt(
            b64decode(encrypted),
            padding.OAEP(
                mgf=padding.MGF1(algorithm=hashes.SHA256()),
                algorithm=hashes.SHA256(),
                label=None,
            ),
        )

    heartbeat_task = None
    result: dict | None = None

    try:
        ws = await ws_connect(REMOTE_AUTH_URL, origin=Origin(DISCORD_ORIGIN))
    except Exception as exc:
        click.echo(f"Failed to connect to Discord: {exc}", err=True)
        return None

    async def send_heartbeat(interval_ms: int) -> None:
        while True:
            await asyncio.sleep(interval_ms / 1000)
            await ws.send(json.dumps({"op": "heartbeat"}))

    try:
        async for raw in ws:
            msg = json.loads(raw)
            op = msg.get("op")

            if op == "hello":
                heartbeat_task = asyncio.create_task(send_heartbeat(msg["heartbeat_interval"]))
                await ws.send(json.dumps({"op": "init", "encoded_public_key": encoded_public_key}))

            elif op == "nonce_proof":
                decrypted = decrypt_payload(msg["encrypted_nonce"])
                proof = urlsafe_b64encode(sha256(decrypted).digest()).decode().rstrip("=")
                await ws.send(json.dumps({"op": "nonce_proof", "proof": proof}))

            elif op == "pending_remote_init":
                fingerprint = msg["fingerprint"]
                qr_url = f"https://discord.com/ra/{fingerprint}"
                _render_qr(qr_url)

            elif op == "pending_ticket":
                decrypted = decrypt_payload(msg["encrypted_user_payload"]).decode()
                parts = decrypted.split(":")
                username = parts[3] if len(parts) > 3 else "unknown"
                click.echo(f"\nUser scanned: {username}")
                click.echo("Waiting for approval on mobile...")

            elif op == "pending_login":
                ticket = msg["ticket"]
                encrypted_token = await _exchange_ticket(ticket)
                if encrypted_token:
                    token = decrypt_payload(encrypted_token).decode()
                    result = {"token": token}
                else:
                    click.echo("Failed to exchange ticket for token.", err=True)
                break

            elif op == "cancel":
                click.echo("Login was cancelled.", err=True)
                break

    except Exception as exc:
        if "4003" in str(exc):
            click.echo("QR code expired. Run again.", err=True)
        else:
            click.echo(f"Login error: {exc}", err=True)
    finally:
        if heartbeat_task:
            heartbeat_task.cancel()
        try:
            await ws.close()
        except Exception:
            pass

    return result


async def _exchange_ticket(ticket: str) -> str | None:
    headers = {"User-Agent": USER_AGENT, "Content-Type": "application/json", "Origin": DISCORD_ORIGIN}
    async with httpx.AsyncClient() as client:
        resp = await client.post(
            "https://discord.com/api/v9/users/@me/remote-auth/login",
            json={"ticket": ticket},
            headers=headers,
        )
        if resp.status_code != 200:
            return None
        return resp.json().get("encrypted_token")


def _render_qr(url: str) -> None:
    """Render QR code in terminal."""
    try:
        import qrcode  # noqa: PLC0415

        qr = qrcode.QRCode(error_correction=qrcode.constants.ERROR_CORRECT_L, box_size=1, border=1)
        qr.add_data(url)
        qr.make(fit=True)

        # Use Unicode block characters for compact terminal rendering
        out = io.StringIO()
        qr.print_ascii(out=out, invert=True)
        click.echo("\nScan this QR code with Discord mobile app:\n")
        click.echo(out.getvalue())
    except ImportError:
        click.echo(f"\nOpen this URL in Discord mobile: {url}")


def _push_token(token: str, email: str) -> None:
    """Push Discord token to auth service as per-user credential."""
    base_url, headers = require_auth()

    if "X-Auth-Secret" in headers:
        url = f"{base_url}/credentials/{email}/discord-mcp"
        body = {"credentials": {"discord_token": token}}
    else:
        url = f"{base_url}/api/extension/credentials/discord-mcp"
        body = {"discord_token": token}

    resp = httpx.post(url, headers=headers, json=body, timeout=15)
    resp.raise_for_status()


@click.group()
def discord() -> None:
    """Manage Discord authentication."""


@discord.command()
@click.option("--email", "-e", default=None, help="User email.")
def login(email: str | None) -> None:
    """Authenticate with Discord via QR code scan.

    Displays a QR code in the terminal. Scan it with the Discord mobile
    app to link your account. The token is stored in the auth service.
    """
    email = email or get_default_email()

    click.echo("Starting Discord QR login...")
    result = asyncio.run(_run_qr_login())

    if not result or "token" not in result:
        raise click.ClickException("Discord login failed")

    click.echo("\nPushing token to auth service...")
    try:
        _push_token(result["token"], email)
    except Exception as e:
        raise click.ClickException(f"Failed to store token: {e}") from e

    click.echo("Done — Discord token stored. Bridge will provision automatically.")
