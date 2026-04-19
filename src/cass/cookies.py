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
from pathlib import Path

import click
import httpx

from cass.config import get_default_email, require_auth


def _open_in_firefox(url: str) -> None:
    """Open a URL in Firefox specifically (cookies are extracted from Firefox)."""
    subprocess.run(["open", "-a", "Firefox", url], capture_output=True)

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


def _validate_cookies_b64(cookies_b64: str, probe_url: str) -> tuple[bool, str]:
    """Validate base64-encoded cookie jar by attempting a yt-dlp fetch.

    Returns (ok, detail) — detail is the video title on success, or error message on failure.
    """
    ytdlp = shutil.which("yt-dlp")
    if not ytdlp:
        return False, "yt-dlp not found"

    cookies_path = Path(tempfile.mkdtemp()) / "validate_cookies.txt"
    try:
        raw = base64.b64decode(cookies_b64)
        cookies_path.write_bytes(raw)
        result = subprocess.run(
            [ytdlp, "--cookies", str(cookies_path), "--skip-download",
             "--print", "title", "--no-warnings", probe_url],
            capture_output=True, text=True, timeout=30,
        )
        if result.returncode == 0 and result.stdout.strip():
            return True, result.stdout.strip()
        return False, result.stderr.strip()[:200] or "yt-dlp failed with no output"
    except Exception as exc:
        return False, str(exc)
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
@click.option("--dry-run", is_flag=True, help="Extract cookies but don't push.")
@click.option("--no-open", is_flag=True,
              help="Don't open login pages on missing/invalid cookies. For unattended use (e.g. session-start hooks).")
def sync(services: tuple[str, ...], dry_run: bool, no_open: bool) -> None:
    """Sync browser cookies to services.

    Specify service names (yt-mcp, twitter, claude-ai) or omit for all.
    Opens login pages automatically if cookies are missing (unless --no-open).
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
        _sync_service(name, svc, dry_run, no_open)

    click.echo()


def _sync_service(name: str, svc: dict, dry_run: bool, no_open: bool = False) -> None:
    """Extract and sync cookies for a single service."""
    def _prompt_login(msg: str) -> None:
        click.echo(f"  {msg}")
        if no_open:
            click.echo("  Sign in to Firefox, then re-run: cass cookies sync " + name)
        else:
            _open_in_firefox(svc["login_url"])
            click.echo("  Sign in, then re-run this command.")

    click.echo("  Extracting from firefox...")
    lines = _extract_cookies_via_ytdlp("firefox", svc["probe_url"])

    if not lines:
        _prompt_login("No cookies found.")
        return

    cookie_names = svc.get("cookie_names")
    if cookie_names:
        # Named cookie mode (twitter: auth_token, ct0)
        creds = _extract_named_cookies(lines, svc["domains"], cookie_names)
        if not creds:
            _prompt_login("Cookies present but missing required keys.")
            return
    else:
        # Full cookie jar mode (yt-mcp, claude-ai)
        filtered = _filter_cookie_lines(lines, svc["domains"])
        if not filtered:
            _prompt_login(f"No cookies for {', '.join(svc['domains'])}.")
            return
        creds = {svc["credential_key"]: _lines_to_jar_b64(filtered)}

    # Validate cookie jar before pushing (yt-mcp, claude-ai — full jar mode)
    cred_key = svc.get("credential_key", "")
    if cred_key in creds and svc.get("probe_url"):
        click.echo("  Validating cookies...")
        ok, detail = _validate_cookies_b64(creds[cred_key], svc["probe_url"])
        if ok:
            click.echo(f"  Valid — {detail}")
        else:
            click.echo(f"  INVALID — {detail}", err=True)
            _prompt_login("Cookies are stale or logged out.")
            return

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
