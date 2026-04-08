"""Cookie sync — extract browser cookies and push to auth service per-service."""

from __future__ import annotations

import base64
import glob
import os
import shutil
import sqlite3
import subprocess
import tempfile
import time
import webbrowser
from pathlib import Path

import click
import httpx

from cass.config import get_default_email, require_auth

# Service definitions: cookie key name, domains to extract from, login URL, extraction method
SERVICES = {
    "yt-mcp": {
        "credential_key": "youtube_cookies",
        "login_url": "https://accounts.google.com/ServiceLogin?service=youtube",
        "description": "YouTube cookies for yt-dlp",
    },
    "twitter": {
        "credential_key": "twitter_cookies",
        "cookie_names": {"auth_token": "twitter_auth_token", "ct0": "twitter_ct0"},
        "domains": [".x.com", ".twitter.com"],
        "login_url": "https://x.com/i/flow/login",
        "description": "Twitter/X auth cookies",
    },
    "claude-ai": {
        "credential_key": "claude_cookies",
        "cookie_names": None,  # extract all cookies for the domain
        "domains": [".claude.ai", "claude.ai"],
        "login_url": "https://claude.ai/login",
        "description": "Claude.ai session cookies",
    },
}


def _find_firefox_cookies_db() -> str | None:
    """Find the default Firefox profile's cookies.sqlite."""
    pattern = os.path.expanduser(
        "~/Library/Application Support/Firefox/Profiles/*/cookies.sqlite"
    )
    paths = glob.glob(pattern)
    return paths[0] if paths else None


def _read_firefox_cookies(domains: list[str], cookie_names: dict[str, str] | None = None) -> dict[str, str]:
    """Read specific cookies from Firefox's cookie store.

    Args:
        domains: List of domain patterns to match (e.g. [".x.com", ".twitter.com"])
        cookie_names: If provided, map of {firefox_cookie_name: credential_key}.
                      If None, extract all cookies for the domains as base64 Netscape jar.
    """
    db_path = _find_firefox_cookies_db()
    if not db_path:
        return {}

    # Copy to avoid locking Firefox's DB
    tmp = tempfile.mktemp(suffix=".sqlite")
    shutil.copy2(db_path, tmp)

    try:
        conn = sqlite3.connect(tmp)
        placeholders = ",".join("?" for _ in domains)

        if cookie_names:
            rows = conn.execute(
                f"SELECT name, value, host FROM moz_cookies WHERE host IN ({placeholders})",
                domains,
            ).fetchall()
            conn.close()
            result = {}
            for name, value, _host in rows:
                if name in cookie_names:
                    result[cookie_names[name]] = value
            return result
        else:
            rows = conn.execute(
                f"SELECT host, path, isSecure, expiry, name, value FROM moz_cookies WHERE host IN ({placeholders})",
                domains,
            ).fetchall()
            conn.close()
            if not rows:
                return {}
            lines = ["# Netscape HTTP Cookie File"]
            for host, path, secure, expiry, name, value in rows:
                secure_str = "TRUE" if secure else "FALSE"
                domain_flag = "TRUE" if host.startswith(".") else "FALSE"
                lines.append(f"{host}\t{domain_flag}\t{path}\t{secure_str}\t{expiry}\t{name}\t{value}")
            jar = "\n".join(lines) + "\n"
            return {"_raw_b64": base64.b64encode(jar.encode()).decode()}
    finally:
        os.unlink(tmp)


def _extract_youtube_cookies(browser: str) -> str | None:
    """Extract YouTube cookies via yt-dlp (returns base64 cookie jar or None)."""
    ytdlp = shutil.which("yt-dlp")
    if not ytdlp:
        return None

    cookies_path = Path(tempfile.mkdtemp()) / "cookies.txt"
    try:
        subprocess.run(
            [ytdlp, "--cookies-from-browser", browser, "--cookies", str(cookies_path),
             "--flat-playlist", "--skip-download", "--no-warnings",
             "https://www.youtube.com/watch?v=dQw4w9WgXcQ"],
            capture_output=True, text=True, timeout=60,
        )
        if not cookies_path.exists() or cookies_path.stat().st_size == 0:
            return None
        raw = cookies_path.read_bytes()
        return base64.b64encode(raw).decode()
    finally:
        cookies_path.unlink(missing_ok=True)


def _push_credentials(service: str, credentials: dict[str, str]) -> None:
    """Push credentials to auth service."""
    base_url, headers = require_auth()
    email = get_default_email()

    if "X-Auth-Secret" in headers:
        url = f"{base_url}/credentials/{email}/{service}"
        body = {"credentials": credentials}
    else:
        url = f"{base_url}/api/extension/credentials/{service}"
        body = credentials

    resp = httpx.put(url, headers=headers, json=body, timeout=15)
    resp.raise_for_status()


@click.group()
def cookies() -> None:
    """Sync browser cookies to platform services."""


@cookies.command()
@click.argument("services", nargs=-1)
@click.option("--browser", "-b", default="firefox", help="Browser to extract cookies from.")
@click.option("--dry-run", is_flag=True, help="Extract cookies but don't push.")
def sync(services: tuple[str, ...], browser: str, dry_run: bool) -> None:
    """Sync browser cookies to services.

    Specify service names (yt-mcp, twitter, claude-ai) or omit for all.
    Opens login pages automatically if cookies are missing.
    """
    targets = list(services) if services else list(SERVICES.keys())

    for name in targets:
        if name not in SERVICES:
            click.echo(f"Unknown service: {name} (available: {', '.join(SERVICES)})")
            continue

        svc = SERVICES[name]
        click.echo(f"\n── {name}: {svc['description']} ──")

        if name == "yt-mcp":
            _sync_youtube(svc, browser, dry_run)
        else:
            _sync_browser_cookies(name, svc, dry_run)

    click.echo()


def _sync_youtube(svc: dict, browser: str, dry_run: bool) -> None:
    """Sync YouTube cookies via yt-dlp."""
    ytdlp = shutil.which("yt-dlp")
    if not ytdlp:
        click.echo("  yt-dlp not found — skipping. Install: brew install yt-dlp")
        return

    click.echo(f"  Extracting from {browser}...")
    cookies_b64 = _extract_youtube_cookies(browser)

    if not cookies_b64:
        click.echo("  No cookies found. Opening YouTube login...")
        webbrowser.open(svc["login_url"])
        click.echo("  Sign in, then re-run this command.")
        return

    size = len(base64.b64decode(cookies_b64))
    click.echo(f"  Extracted {size} bytes")

    if dry_run:
        click.echo("  Dry run — not pushing.")
        return

    _push_credentials("yt-mcp", {"youtube_cookies": cookies_b64})
    click.echo("  Synced ✓")


def _sync_browser_cookies(name: str, svc: dict, dry_run: bool) -> None:
    """Sync cookies read directly from Firefox's cookie store."""
    click.echo("  Reading Firefox cookies...")

    domains = svc["domains"]
    cookie_names = svc.get("cookie_names")
    creds = _read_firefox_cookies(domains, cookie_names)

    if not creds:
        click.echo(f"  No cookies found. Opening login page...")
        webbrowser.open(svc["login_url"])
        click.echo("  Sign in in the browser, then re-run this command.")
        return

    # For raw cookie jar mode (claude-ai), wrap the base64
    if "_raw_b64" in creds:
        creds = {svc["credential_key"]: creds["_raw_b64"]}

    if dry_run:
        keys = ", ".join(creds.keys())
        click.echo(f"  Found: {keys}")
        click.echo("  Dry run — not pushing.")
        return

    _push_credentials(name, creds)
    keys = ", ".join(creds.keys())
    click.echo(f"  Synced: {keys} ✓")


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
