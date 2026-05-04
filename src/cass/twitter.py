"""Sync X (Twitter) GraphQL queryIds to the auth service.

X rotates GraphQL operation IDs on its web frontend; pinned scrapers
break the moment an ID drifts. This command scrapes a logged-in
Firefox session, extracts current (operationName, queryId) pairs from
x.com's main JS bundles, and PUTs them to a shared service-credentials
record. The twitter-mcp backend polls that record on an interval and
patches `twitter_cli.graphql._cached_query_ids` in place.
"""

from __future__ import annotations

import base64
import re
import shutil
import subprocess
import tempfile
from pathlib import Path

import click
import httpx

from cass.config import get_auth_secret, get_auth_url

SHARED_SERVICE_KEY = "twitter-mcp-queryids"

# Match `<script src="…/main.<hash>.js">` and similar tags emitted by x.com.
_BUNDLE_URL_PATTERN = re.compile(
    r'(?:src|href)=["\']'
    r'(https://abs\.twimg\.com/responsive-web/client-web[^"\']+\.js)'
    r'["\']'
)
# Match `queryId:"…",…operationName:"…"` pairs in minified JS.
_OP_PATTERN = re.compile(
    r'queryId:\s*"([A-Za-z0-9_-]+)"[^}]{0,200}operationName:\s*"([^"]+)"'
)
_USER_AGENT = (
    "Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:135.0) "
    "Gecko/20100101 Firefox/135.0"
)


def _firefox_cookies_for_x() -> dict[str, str]:
    """Pull x.com / twitter.com cookies from Firefox via yt-dlp into a dict."""
    ytdlp = shutil.which("yt-dlp")
    if not ytdlp:
        raise click.ClickException("yt-dlp is required. Install: brew install yt-dlp")

    out = Path(tempfile.mkdtemp()) / "cookies.txt"
    try:
        subprocess.run(
            [ytdlp, "--cookies-from-browser", "firefox", "--cookies", str(out),
             "--flat-playlist", "--skip-download", "--no-warnings", "https://x.com"],
            capture_output=True, text=True, timeout=60,
        )
        if not out.exists() or out.stat().st_size == 0:
            return {}
        jar: dict[str, str] = {}
        for line in out.read_text().splitlines():
            if line.startswith("#") or not line.strip():
                continue
            parts = line.split("\t")
            if len(parts) < 7:
                continue
            host, name, value = parts[0], parts[5], parts[6]
            if host.endswith(".x.com") or host.endswith(".twitter.com"):
                jar[name] = value
        return jar
    finally:
        out.unlink(missing_ok=True)


def _scrape_query_ids(cookies: dict[str, str]) -> dict[str, str]:
    """Hit x.com, find bundle URLs, extract queryId/operationName pairs."""
    headers = {"User-Agent": _USER_AGENT, "Accept-Language": "en-US,en;q=0.5"}

    with httpx.Client(http2=False, timeout=30, follow_redirects=True) as client:
        home = client.get("https://x.com/", headers=headers, cookies=cookies)
        home.raise_for_status()
        bundle_urls = list(dict.fromkeys(_BUNDLE_URL_PATTERN.findall(home.text)))
        if not bundle_urls:
            raise click.ClickException(
                "No JS bundles found on x.com — cookies may be stale. "
                "Run: cass cookies sync twitter"
            )

        result: dict[str, str] = {}
        for url in bundle_urls:
            try:
                resp = client.get(url, headers=headers)
                if resp.status_code != 200:
                    continue
                for query_id, op_name in _OP_PATTERN.findall(resp.text):
                    result.setdefault(op_name, query_id)
            except Exception as exc:
                click.echo(f"  bundle fetch failed ({url.rsplit('/', 1)[-1]}): {exc}", err=True)

    return result


def _push(query_ids: dict[str, str]) -> None:
    secret = get_auth_secret()
    if not secret:
        raise click.ClickException(
            "AUTH_SECRET not available — service-credentials writes need the "
            "shared admin secret (env/acl.env)."
        )
    url = f"{get_auth_url()}/service-credentials/{SHARED_SERVICE_KEY}"
    headers = {"X-Auth-Secret": secret, "Content-Type": "application/json"}
    resp = httpx.post(url, headers=headers, json={"queryIds": query_ids}, timeout=15)
    resp.raise_for_status()


@click.group()
def twitter() -> None:
    """X / Twitter helper commands."""


@twitter.command("sync-queryids")
@click.option("--dry-run", is_flag=True, help="Scrape and print, don't push.")
def sync_queryids(dry_run: bool) -> None:
    """Scrape current X GraphQL queryIds from Firefox and push to auth."""
    click.echo("Pulling x.com cookies from firefox...")
    cookies = _firefox_cookies_for_x()
    if not cookies:
        raise click.ClickException(
            "No x.com cookies in firefox. Run: cass cookies sync twitter"
        )

    click.echo("Scraping x.com JS bundles for queryIds...")
    query_ids = _scrape_query_ids(cookies)
    if not query_ids:
        raise click.ClickException("No queryIds extracted — bundle layout may have changed.")

    click.echo(f"  Found {len(query_ids)} operations.")
    for op in sorted(query_ids):
        click.echo(f"    {op:32s} {query_ids[op]}")

    if dry_run:
        click.echo("Dry run — not pushing.")
        return

    _push(query_ids)
    click.echo(f"Pushed to /service-credentials/{SHARED_SERVICE_KEY} ✓")
