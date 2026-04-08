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

# Service definitions
SERVICES = {
    "yt-mcp": {
        "credential_key": "youtube_cookies",
        "domains": [".youtube.com", ".google.com"],
        "login_url": "https://accounts.google.com/ServiceLogin?service=youtube",
        "probe_url": "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
        "description": "YouTube cookies for yt-dlp",
    },
    "twitter": {
        "credential_key": "twitter_cookies",
        "cookie_names": {"auth_token": "twitter_auth_token", "ct0": "twitter_ct0"},
        "domains": [".x.com", ".twitter.com"],
        "login_url": "https://x.com/i/flow/login",
        "probe_url": "https://x.com",
        "description": "Twitter/X auth cookies",
    },
    "claude-ai": {
        "credential_key": "claude_cookies",
        "domains": [".claude.ai", "claude.ai"],
        "login_url": "https://claude.ai/login",
        "probe_url": "https://claude.ai",
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


def _check_firefox_cookies(domains: list[str], required_names: list[str] | None = None) -> bool:
    """Fast check if Firefox has cookies for the given domains. No extraction, no yt-dlp."""
    db_path = _find_firefox_cookies_db()
    if not db_path:
        return False

    tmp = tempfile.mktemp(suffix=".sqlite")
    shutil.copy2(db_path, tmp)
    try:
        conn = sqlite3.connect(tmp)
        placeholders = ",".join("?" for _ in domains)
        now = int(time.time())

        if required_names:
            # Check specific cookie names exist and aren't expired
            name_placeholders = ",".join("?" for _ in required_names)
            row = conn.execute(
                f"SELECT COUNT(DISTINCT name) FROM moz_cookies "
                f"WHERE host IN ({placeholders}) AND name IN ({name_placeholders}) "
                f"AND (expiry = 0 OR expiry > ?)",
                [*domains, *required_names, now],
            ).fetchone()
            conn.close()
            return row[0] == len(required_names)
        else:
            # Check any non-expired cookies exist for domains
            row = conn.execute(
                f"SELECT COUNT(*) FROM moz_cookies "
                f"WHERE host IN ({placeholders}) AND (expiry = 0 OR expiry > ?)",
                [*domains, now],
            ).fetchone()
            conn.close()
            return row[0] > 0
    except Exception:
        return False
    finally:
        os.unlink(tmp)


def _extract_cookies_via_ytdlp(browser: str, probe_url: str) -> list[str]:
    """Extract all browser cookies via yt-dlp, returns lines from the Netscape cookie file."""
    ytdlp = shutil.which("yt-dlp")
    if not ytdlp:
        return []

    cookies_path = Path(tempfile.mkdtemp()) / "cookies.txt"
    try:
        subprocess.run(
            [ytdlp, "--cookies-from-browser", browser, "--cookies", str(cookies_path),
             "--flat-playlist", "--skip-download", "--no-warnings", probe_url],
            capture_output=True, text=True, timeout=60,
        )
        if not cookies_path.exists() or cookies_path.stat().st_size == 0:
            return []
        return cookies_path.read_text().splitlines()
    finally:
        cookies_path.unlink(missing_ok=True)


def _filter_cookie_lines(lines: list[str], domains: list[str]) -> list[str]:
    """Filter Netscape cookie jar lines to only those matching given domains."""
    filtered = []
    for line in lines:
        if line.startswith("#") or not line.strip():
            continue
        parts = line.split("\t")
        if len(parts) < 7:
            continue
        host = parts[0]
        if any(host == d or host.endswith(d) for d in domains):
            filtered.append(line)
    return filtered


def _lines_to_jar_b64(lines: list[str]) -> str:
    """Convert filtered cookie lines to a base64-encoded Netscape cookie jar."""
    jar = "# Netscape HTTP Cookie File\n" + "\n".join(lines) + "\n"
    return base64.b64encode(jar.encode()).decode()


def _extract_named_cookies(lines: list[str], domains: list[str],
                           cookie_names: dict[str, str]) -> dict[str, str]:
    """Extract specific named cookies from Netscape cookie jar lines.

    Args:
        cookie_names: map of {cookie_name_in_jar: credential_key}
    """
    result = {}
    for line in lines:
        if line.startswith("#") or not line.strip():
            continue
        parts = line.split("\t")
        if len(parts) < 7:
            continue
        host, name, value = parts[0], parts[5], parts[6]
        if not any(host == d or host.endswith(d) for d in domains):
            continue
        if name in cookie_names:
            result[cookie_names[name]] = value
    return result


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
@click.option("--dry-run", is_flag=True, help="Extract cookies but don't push.")
def sync(services: tuple[str, ...], dry_run: bool) -> None:
    """Sync browser cookies to services.

    Specify service names (yt-mcp, twitter, claude-ai) or omit for all.
    Opens login pages automatically if cookies are missing.
    """
    ytdlp = shutil.which("yt-dlp")
    if not ytdlp:
        raise click.ClickException("yt-dlp is required. Install: brew install yt-dlp")

    targets = list(services) if services else list(SERVICES.keys())

    for name in targets:
        if name not in SERVICES:
            click.echo(f"Unknown service: {name} (available: {', '.join(SERVICES)})")
            continue

        svc = SERVICES[name]
        click.echo(f"\n── {name}: {svc['description']} ──")
        _sync_service(name, svc, dry_run)

    click.echo()


def _sync_service(name: str, svc: dict, dry_run: bool) -> None:
    """Extract and sync cookies for a single service."""
    click.echo("  Extracting from firefox...")
    lines = _extract_cookies_via_ytdlp("firefox", svc["probe_url"])

    if not lines:
        click.echo(f"  No cookies found. Opening login page...")
        webbrowser.open(svc["login_url"])
        click.echo("  Sign in, then re-run this command.")
        return

    cookie_names = svc.get("cookie_names")
    if cookie_names:
        # Named cookie mode (twitter: auth_token, ct0)
        creds = _extract_named_cookies(lines, svc["domains"], cookie_names)
        if not creds:
            click.echo(f"  Cookies present but missing required keys. Opening login page...")
            webbrowser.open(svc["login_url"])
            click.echo("  Sign in, then re-run this command.")
            return
    else:
        # Full cookie jar mode (yt-mcp, claude-ai)
        filtered = _filter_cookie_lines(lines, svc["domains"])
        if not filtered:
            click.echo(f"  No cookies for {', '.join(svc['domains'])}. Opening login page...")
            webbrowser.open(svc["login_url"])
            click.echo("  Sign in, then re-run this command.")
            return
        creds = {svc["credential_key"]: _lines_to_jar_b64(filtered)}

    if dry_run:
        keys = ", ".join(creds.keys())
        click.echo(f"  Found: {keys}")
        click.echo("  Dry run — not pushing.")
        return

    _push_credentials(name, creds)
    keys = ", ".join(creds.keys())
    click.echo(f"  Synced: {keys} ✓")


@cookies.command()
def status() -> None:
    """Check which services have valid cookies in Firefox (fast, no network)."""
    missing = []
    for name, svc in SERVICES.items():
        required = list(svc["cookie_names"].keys()) if svc.get("cookie_names") else None
        ok = _check_firefox_cookies(svc["domains"], required)
        icon = "ok" if ok else "MISSING"
        click.echo(f"  {name:12s} {icon}")
        if not ok:
            missing.append(name)

    if missing:
        click.echo(f"\nRun: cass cookies sync {' '.join(missing)}")
        raise SystemExit(1)


@cookies.command()
def test() -> None:
    """Test that yt-dlp can fetch a YouTube page with Firefox cookies."""
    ytdlp = shutil.which("yt-dlp")
    if not ytdlp:
        raise click.ClickException("yt-dlp not found in PATH")

    click.echo("Testing yt-dlp with firefox cookies...")
    result = subprocess.run(
        [ytdlp, "--cookies-from-browser", "firefox", "--skip-download",
         "--print", "title", "https://www.youtube.com/watch?v=dQw4w9WgXcQ"],
        capture_output=True, text=True, timeout=30,
    )
    if result.returncode != 0:
        click.echo(f"FAIL: {result.stderr.strip()}", err=True)
        raise SystemExit(1)
    click.echo(f"OK — got title: {result.stdout.strip()}")
