"""Shared config — auth and API access."""

from __future__ import annotations

import os
from pathlib import Path

import click

PORTAL_URL = "https://portal.cassandrasedge.com"

# Look for env vars first, then fall back to reading env files from cassandra-stack/env/
_STACK_ROOT = Path(__file__).resolve().parents[3]  # cass-cli/src/cass -> cassandra-stack
_ACL_ENV = _STACK_ROOT / "env" / "acl.env"
_PORTAL_ENV = _STACK_ROOT / "env" / "portal.env"
_SCHWAB_ENV = _STACK_ROOT / "env" / "schwab.local.env"


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


def _read_local_env() -> dict[str, str]:
    merged: dict[str, str] = {}
    for path in (_ACL_ENV, _PORTAL_ENV, _SCHWAB_ENV):
        merged.update(_read_env_file(path))
    return merged


def get_auth_url() -> str:
    """Auth service URL — only available when AUTH_SECRET is set (cluster/local)."""
    url = os.environ.get("AUTH_URL")
    if url:
        return url
    return _read_local_env().get("AUTH_URL", "https://auth.cassandrasedge.com")


def get_auth_secret() -> str | None:
    """Auth secret for direct auth service access. Returns None if not available."""
    secret = os.environ.get("AUTH_SECRET")
    if secret:
        return secret
    return _read_local_env().get("AUTH_SECRET")


def get_default_email() -> str:
    local = _read_local_env()
    return os.environ.get("CASS_EMAIL") or local.get("CASS_EMAIL") or local.get("DEFAULT_USER_EMAIL") or "andrew@raftesalo.net"


def get_portal_url() -> str:
    local = _read_local_env()
    return os.environ.get("CASS_PORTAL_URL") or local.get("CASS_PORTAL_URL") or PORTAL_URL


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


def _get_cf_service_token() -> tuple[str, str] | None:
    """CF Access service token (Client ID + Secret) from env or local env files.

    Set as a pair: CF_ACCESS_CLIENT_ID + CF_ACCESS_CLIENT_SECRET.
    Mints once via Terraform (cassandra-infra), set in shell profile
    forever after. Bypasses the interactive WorkOS login flow entirely
    and never expires unless rotated.
    """
    local = _read_local_env()
    cid = os.environ.get("CF_ACCESS_CLIENT_ID") or local.get("CF_ACCESS_CLIENT_ID")
    csec = os.environ.get("CF_ACCESS_CLIENT_SECRET") or local.get("CF_ACCESS_CLIENT_SECRET")
    if cid and csec:
        return cid, csec
    return None


def require_auth() -> tuple[str, dict[str, str]]:
    """Get base URL and auth headers for API calls.

    Auth resolution order:
      1. Direct mode: AUTH_SECRET + auth URL reachable (in-cluster / dev).
      2. Service token mode: CF_ACCESS_CLIENT_ID + CF_ACCESS_CLIENT_SECRET
         set in env. Long-lived, never prompts, machine-friendly.
      3. Portal mode: cached MCP key + interactive WorkOS-backed CF cookie
         from `cass login`. Last resort — short-lived, requires browser.

    Returns (base_url, headers).
    """
    # Direct mode: AUTH_SECRET available and auth URL reachable (dev/cluster)
    secret = get_auth_secret()
    if secret:
        auth_url = get_auth_url()
        if _is_reachable(auth_url):
            return auth_url, {"X-Auth-Secret": secret, "Content-Type": "application/json"}

    # Service-token mode: prefer over WorkOS interactive flow when env is set.
    portal = get_portal_url()
    if (svc := _get_cf_service_token()) is not None:
        cid, csec = svc
        # Still need a Bearer for the inner auth check on credential routes;
        # service token covers the CF Access edge, MCP key covers the API call.
        from cass.ensure import get_service_key  # noqa: PLC0415
        # Try cached key for a stable default service.
        bearer = get_service_key("perplexity-mcp") or get_service_key("market-research") or ""
        headers: dict[str, str] = {
            "CF-Access-Client-Id": cid,
            "CF-Access-Client-Secret": csec,
            "Content-Type": "application/json",
        }
        if bearer:
            headers["Authorization"] = f"Bearer {bearer}"
        return portal, headers

    # Portal mode: cached MCP key + CF Access JWT from `cass login`
    from cass.auth import ensure_auth  # noqa: PLC0415

    auth = ensure_auth()

    headers = {
        "Authorization": f"Bearer {auth['key']}",
        "Content-Type": "application/json",
    }
    if auth.get("cf_token"):
        headers["Cookie"] = f"CF_Authorization={auth['cf_token']}"
    return portal, headers
