"""Shared config — auth and API access."""

from __future__ import annotations

import os
from pathlib import Path

import click

PORTAL_URL = "https://portal.cassandrasedge.com"

# CF Access service token for programmatic portal access (bypasses CF Access OAuth)
_CF_ACCESS_CLIENT_ID = "df4eae9c073d0f09b8eb23d42d9499bd.access"
_CF_ACCESS_CLIENT_SECRET = "dbb089c2798d720003fbd0a305ac85161c450b10a1bb02e3b45a0bb276c03aaa"

# Look for env vars first, then fall back to reading env files from cassandra-stack/env/
_STACK_ROOT = Path(__file__).resolve().parents[4]  # toolbox/cass/src/cass -> cassandra-stack
_ACL_ENV = _STACK_ROOT / "env" / "acl.env"


def _read_env_file(path: Path) -> dict[str, str]:
    """Parse KEY=VALUE lines from an env file."""
    if not path.exists():
        return {}
    vals: dict[str, str] = {}
    for line in path.read_text().splitlines():
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        if "=" in line:
            k, v = line.split("=", 1)
            vals[k.strip()] = v.strip()
    return vals


def get_auth_url() -> str:
    """Auth service URL — only available when AUTH_SECRET is set (cluster/local)."""
    url = os.environ.get("AUTH_URL")
    if url:
        return url
    return _read_env_file(_ACL_ENV).get("AUTH_URL", "https://auth.cassandrasedge.com")


def get_auth_secret() -> str | None:
    """Auth secret for direct auth service access. Returns None if not available."""
    secret = os.environ.get("AUTH_SECRET")
    if secret:
        return secret
    return _read_env_file(_ACL_ENV).get("AUTH_SECRET")


def get_default_email() -> str:
    return os.environ.get("CASS_EMAIL", "andrew@raftesalo.net")


def get_portal_url() -> str:
    return os.environ.get("CASS_PORTAL_URL", PORTAL_URL)


def _is_reachable(url: str) -> bool:
    """Quick check if a URL's host is resolvable."""
    import socket  # noqa: PLC0415
    from urllib.parse import urlparse  # noqa: PLC0415

    try:
        host = urlparse(url).hostname
        if not host:
            return False
        socket.getaddrinfo(host, None, socket.AF_UNSPEC, socket.SOCK_STREAM)
        return True
    except socket.gaierror:
        return False


def require_auth() -> tuple[str, dict[str, str]]:
    """Get base URL and auth headers for API calls.

    Tries direct auth service first (AUTH_SECRET + reachable URL),
    falls back to portal with cached MCP key.
    Returns (base_url, headers).
    """
    # Direct mode: AUTH_SECRET available and auth URL reachable (dev/cluster)
    secret = get_auth_secret()
    if secret:
        auth_url = get_auth_url()
        if _is_reachable(auth_url):
            return auth_url, {"X-Auth-Secret": secret, "Content-Type": "application/json"}

    # Portal mode: cached MCP key from `cass login`
    from cass.auth import get_cached_auth  # noqa: PLC0415

    auth = get_cached_auth()
    if not auth:
        raise click.ClickException("Not authenticated. Run: cass login")

    portal = get_portal_url()
    headers: dict[str, str] = {
        "Authorization": f"Bearer {auth['key']}",
        "Content-Type": "application/json",
        # CF Access service token — bypasses OAuth redirect for programmatic access
        "CF-Access-Client-Id": _CF_ACCESS_CLIENT_ID,
        "CF-Access-Client-Secret": _CF_ACCESS_CLIENT_SECRET,
    }
    return portal, headers
