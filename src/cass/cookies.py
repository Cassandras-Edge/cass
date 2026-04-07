"""Cookie sync — extract browser cookies via yt-dlp and push to auth service."""

from __future__ import annotations

import base64
import shutil
import subprocess
import tempfile
from pathlib import Path

import click
import httpx

from cass.config import get_default_email, require_auth


@click.group()
def cookies() -> None:
    """Manage YouTube cookies for yt-dlp."""


@cookies.command()
@click.option("--browser", "-b", default="firefox", help="Browser to extract cookies from.")
@click.option("--email", "-e", default=None, help="User email (default: andrew@raftesalo.net).")
@click.option("--service", "-s", default="yt-mcp", help="Service to push credentials to.")
@click.option("--dry-run", is_flag=True, help="Extract cookies but don't push to auth service.")
def sync(browser: str, email: str | None, service: str, dry_run: bool) -> None:
    """Extract YouTube cookies from browser and push to auth service."""
    email = email or get_default_email()

    ytdlp = shutil.which("yt-dlp")
    if not ytdlp:
        raise click.ClickException("yt-dlp not found in PATH")

    cookies_path = Path(tempfile.mkdtemp()) / "cookies.txt"

    try:
        click.echo(f"Extracting cookies from {browser}...")
        # Use --flat-playlist and a known URL — yt-dlp writes cookies before any network.
        # The command may "fail" on the URL but cookies are still written.
        subprocess.run(
            [ytdlp, "--cookies-from-browser", browser, "--cookies", str(cookies_path),
             "--flat-playlist", "--skip-download", "--no-warnings",
             "https://www.youtube.com/watch?v=dQw4w9WgXcQ"],
            capture_output=True, text=True, timeout=60,
        )

        if not cookies_path.exists() or cookies_path.stat().st_size == 0:
            raise click.ClickException("No cookies extracted — is the browser installed with YouTube cookies?")

        raw = cookies_path.read_bytes()
        cookies_b64 = base64.b64encode(raw).decode()
        click.echo(f"Extracted {len(raw)} bytes ({len(raw.splitlines())} cookie lines)")

        if dry_run:
            click.echo("Dry run — not pushing.")
            return

        base_url, headers = require_auth()

        # Direct auth service mode
        if "X-Auth-Secret" in headers:
            url = f"{base_url}/credentials/{email}/{service}"
            body = {"credentials": {"youtube_cookies": cookies_b64}}
        else:
            # Portal mode — use extension endpoint
            url = f"{base_url}/api/extension/credentials/{service}"
            body = {"youtube_cookies": cookies_b64}

        click.echo(f"Pushing to {service}...")
        resp = httpx.post(url, headers=headers, json=body, timeout=15)
        resp.raise_for_status()
        click.echo("Done — cookies synced.")

    finally:
        cookies_path.unlink(missing_ok=True)


@cookies.command()
@click.option("--browser", "-b", default="firefox", help="Browser to extract cookies from.")
def test(browser: str) -> None:
    """Test that yt-dlp can fetch a YouTube page with current browser cookies."""
    ytdlp = shutil.which("yt-dlp")
    if not ytdlp:
        raise click.ClickException("yt-dlp not found in PATH")

    click.echo(f"Testing yt-dlp with {browser} cookies...")
    result = subprocess.run(
        [ytdlp, "--cookies-from-browser", browser, "--skip-download",
         "--print", "title", "https://www.youtube.com/watch?v=dQw4w9WgXcQ"],
        capture_output=True, text=True, timeout=30,
    )
    if result.returncode != 0:
        click.echo(f"FAIL: {result.stderr.strip()}", err=True)
        raise SystemExit(1)
    click.echo(f"OK — got title: {result.stdout.strip()}")
