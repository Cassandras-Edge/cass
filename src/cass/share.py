"""`cass share` — ephemeral share links for Claude Code conversations.

Turns a Claude Code session .jsonl into sanitized markdown, uploads to the
cassandra-share service, and prints a clipboard-ready message.

Subcommands:
  cass share create [SESSION]  — create a share, print clipboard text
  cass share list              — list your active shares
  cass share revoke TOKEN      — revoke early
"""
from __future__ import annotations

import json
import os
import re
import subprocess
import sys
from pathlib import Path

import click
import httpx

from cass.auth import ensure_auth


SHARE_URL_DEFAULT = "https://share.cassandrasedge.com"  # admin host (CF Access)


def _share_url() -> str:
    """Base URL for the share service's admin routes.

    Public capability URLs (returned by the server) use a separate
    SHARE_PUBLIC_DOMAIN and are generated server-side — the client doesn't need
    to know about them.
    """
    return os.environ.get("CASS_SHARE_URL") or SHARE_URL_DEFAULT


def _share_headers() -> dict[str, str]:
    """Auth headers for the share service. Requires CF Access login."""
    auth = ensure_auth()
    headers: dict[str, str] = {"Content-Type": "application/json"}
    if auth.get("cf_token"):
        headers["Cf-Access-Jwt-Assertion"] = auth["cf_token"]
    # Dev shortcut: if running against a local share service without CF Access,
    # the service accepts X-Dev-Email.
    if os.environ.get("CASS_DEV_EMAIL"):
        headers["X-Dev-Email"] = os.environ["CASS_DEV_EMAIL"]
    elif auth.get("email"):
        headers["X-Dev-Email"] = auth["email"]
    return headers


# ── Session discovery ─────────────────────────────────────────────────────


def _resolve_session_path(arg: str | None) -> Path:
    """Find the Claude Code session JSONL to share.

    Resolution order:
      1. explicit path argument (if given)
      2. $CLAUDE_SESSION_ID + $CLAUDE_PROJECT_DIR lookup
      3. newest .jsonl in the current-cwd-hashed project dir
    """
    if arg:
        p = Path(arg).expanduser()
        if not p.exists():
            raise click.ClickException(f"No such file: {p}")
        return p

    base = Path.home() / ".claude" / "projects"
    if not base.exists():
        raise click.ClickException(f"{base} missing — Claude Code not set up on this machine")

    session_id = os.environ.get("CLAUDE_SESSION_ID")
    if session_id:
        for project_dir in base.iterdir():
            candidate = project_dir / f"{session_id}.jsonl"
            if candidate.exists():
                return candidate

    # Fallback: newest jsonl under the hashed dir for this cwd.
    cwd_hash_dir = _project_dir_for_cwd(base)
    if cwd_hash_dir and cwd_hash_dir.is_dir():
        candidates = sorted(cwd_hash_dir.glob("*.jsonl"), key=lambda p: p.stat().st_mtime, reverse=True)
        if candidates:
            return candidates[0]

    raise click.ClickException(
        "Could not locate the current session .jsonl. "
        "Pass the path explicitly: cass share create /path/to/session.jsonl"
    )


def _project_dir_for_cwd(base: Path) -> Path | None:
    """Claude Code stores sessions under a directory whose name is the cwd path
    with slashes replaced by hyphens. Try to match the current cwd."""
    cwd = os.getcwd()
    mangled = "-" + cwd.replace("/", "-")
    candidate = base / mangled
    if candidate.exists():
        return candidate
    return None


# ── JSONL → markdown + local sanitizer ────────────────────────────────────


SECRET_PATTERNS = [
    (re.compile(r"sk-[A-Za-z0-9_\-]{20,}"), "<OPENAI_KEY>"),
    (re.compile(r"ghp_[A-Za-z0-9]{30,}"), "<GITHUB_TOKEN>"),
    (re.compile(r"AKIA[0-9A-Z]{16}"), "<AWS_ACCESS_KEY_ID>"),
    (re.compile(r"AIza[0-9A-Za-z_\-]{35}"), "<GOOGLE_API_KEY>"),
    (re.compile(r"sk_live_[A-Za-z0-9]{20,}"), "<STRIPE_KEY>"),
    (re.compile(r"-----BEGIN [A-Z ]+PRIVATE KEY-----[\s\S]+?-----END [A-Z ]+PRIVATE KEY-----"),
     "<PRIVATE_KEY_PEM>"),
    (re.compile(r"eyJ[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+"), "<JWT>"),
]
PATH_PATTERN = re.compile(r"/Users/[a-zA-Z0-9._-]+")
HOSTNAME_PATTERN = re.compile(r"\b(?:\d{1,3}\.){3}\d{1,3}\b")


def _sanitize(text: str) -> str:
    """Local regex sanitizer. Catches well-known credential formats, absolute
    home paths, and private IPs — zero dependencies, always runs.

    For deeper context-aware scanning (JWTs in prose, PEMs across line breaks,
    passwords inside URLs), set CASS_SHARE_DEEP_SCAN=1 to additionally run
    OpenAI Privacy Filter (OPF) via the separate cassandra-scrub endpoint.
    OPF is slower (seconds per 100k tokens on CUDA) but catches things regex
    can't describe. If OPF isn't reachable, we fall back silently to regex-only.
    """
    for pat, repl in SECRET_PATTERNS:
        text = pat.sub(repl, text)
    text = PATH_PATTERN.sub("<HOME>", text)
    text = HOSTNAME_PATTERN.sub("<INTERNAL_IP>", text)

    if os.environ.get("CASS_SHARE_DEEP_SCAN"):
        text = _opf_sanitize(text)
    return text


def _opf_sanitize(text: str) -> str:
    """Deep scan via OPF. No-op if the scrub service isn't configured or fails.

    Contract: POSTs the text to CASS_SCRUB_URL (or a future `cassandra-scrub`
    deployment), which runs OPF server-side and returns the redacted text.
    Keeping the network call optional lets cass ship without a heavy local
    model dependency.
    """
    scrub_url = os.environ.get("CASS_SCRUB_URL")
    if not scrub_url:
        return text
    try:
        with httpx.Client(timeout=60) as client:
            resp = client.post(scrub_url, json={"text": text})
            resp.raise_for_status()
            return resp.json().get("redacted", text)
    except Exception as e:
        click.echo(f"(deep-scan unavailable: {e} — using regex-only)", err=True)
        return text


def _extract_content(record: dict) -> list[tuple[str, str]]:
    """Flatten one record's content into (kind, text) tuples."""
    m = record.get("message")
    if not isinstance(m, dict):
        return []
    role = str(m.get("role", "unknown"))
    content = m.get("content")
    if isinstance(content, str):
        return [(role, content)]
    if not isinstance(content, list):
        return []
    out: list[tuple[str, str]] = []
    for part in content:
        if not isinstance(part, dict):
            continue
        t = part.get("type")
        if t == "text" or (t is None and "text" in part):
            out.append((role, str(part.get("text", ""))))
        elif t == "tool_use":
            name = part.get("name", "tool")
            inp = part.get("input")
            try:
                serial = json.dumps(inp, ensure_ascii=False, indent=2)
            except TypeError:
                serial = str(inp)
            out.append((f"{role}:tool_use:{name}", serial))
        elif t == "tool_result":
            tc = part.get("content")
            if isinstance(tc, str):
                out.append((f"{role}:tool_result", tc))
            elif isinstance(tc, list):
                for y in tc:
                    if isinstance(y, dict) and isinstance(y.get("text"), str):
                        out.append((f"{role}:tool_result", y["text"]))
    return out


def _jsonl_to_markdown(jsonl_path: Path, title: str | None = None) -> str:
    """Render a Claude Code session .jsonl as an LLM-pasteable markdown transcript."""
    lines: list[str] = []
    if title:
        lines.append(f"# {title}")
        lines.append("")

    with jsonl_path.open("r") as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            try:
                rec = json.loads(line)
            except json.JSONDecodeError:
                continue
            for kind, text in _extract_content(rec):
                if not text.strip():
                    continue
                text = _sanitize(text)
                if kind.endswith(":tool_use") or ":tool_use:" in kind:
                    lines.append(f"### {kind}")
                    lines.append("```json")
                    lines.append(text)
                    lines.append("```")
                elif kind.endswith(":tool_result"):
                    lines.append(f"### {kind}")
                    lines.append("```")
                    lines.append(text[:8000])  # cap giant outputs
                    if len(text) > 8000:
                        lines.append(f"... ({len(text) - 8000} more chars truncated)")
                    lines.append("```")
                else:
                    lines.append(f"## {kind}")
                    lines.append(text)
                lines.append("")
    return "\n".join(lines).strip() + "\n"


# ── TTL parsing ────────────────────────────────────────────────────────────


def _parse_ttl(ttl: str) -> int:
    """Parse '24h', '6h', '7d' into hours."""
    m = re.fullmatch(r"(\d+)([hd])", ttl.strip().lower())
    if not m:
        raise click.BadParameter(f"invalid --ttl {ttl!r}; use forms like 6h, 24h, 7d")
    n, unit = int(m.group(1)), m.group(2)
    return n if unit == "h" else n * 24


# ── Clipboard ──────────────────────────────────────────────────────────────


def _copy(text: str) -> bool:
    """Copy to macOS clipboard via pbcopy. Returns True on success."""
    try:
        p = subprocess.run(["pbcopy"], input=text, text=True, check=True)
        return p.returncode == 0
    except (FileNotFoundError, subprocess.CalledProcessError):
        return False


# ── Commands ───────────────────────────────────────────────────────────────


@click.group()
def share() -> None:
    """Share Claude Code conversations via ephemeral URLs."""


@share.command("create")
@click.argument("session", required=False)
@click.option("--ttl", default="24h", help="Expiry: e.g. 6h, 24h, 7d. Default 24h.")
@click.option("--once", is_flag=True, help="Self-destruct after first fetch.")
@click.option("--title", default=None, help="Optional human title for listing.")
@click.option("--summary", default=None,
              help="2-3 line 'About:' blurb. If omitted, first turn's first 200 chars.")
@click.option("--no-copy", is_flag=True, help="Skip copying to clipboard.")
def create_cmd(session: str | None, ttl: str, once: bool,
               title: str | None, summary: str | None, no_copy: bool) -> None:
    """Upload the current (or specified) session as a share link."""
    jsonl_path = _resolve_session_path(session)
    click.echo(f"Reading {jsonl_path}...", err=True)
    body = _jsonl_to_markdown(jsonl_path, title=title)
    click.echo(f"  → {len(body):,} chars of sanitized markdown", err=True)

    if summary is None:
        # Crude default: first non-empty user line.
        for line in body.splitlines():
            s = line.strip()
            if s and not s.startswith("#") and not s.startswith("```"):
                summary = s[:200]
                break
        summary = summary or "(no summary)"

    hours = _parse_ttl(ttl)
    payload = {
        "body": body,
        "title": title,
        "summary": summary,
        "ttl_hours": hours,
        "once": once,
    }
    with httpx.Client(base_url=_share_url(), timeout=30, headers=_share_headers()) as client:
        resp = client.post("/share", json=payload)
        if resp.status_code != 200:
            raise click.ClickException(f"{resp.status_code}: {resp.text}")
        data = resp.json()

    url = data["url"]
    expires = data["expires_at"]
    expiry_note = f"expires {expires}" + (" — single-use" if once else "")

    clipboard_text = (
        f"Continue this Claude convo ({expiry_note}):\n"
        f"curl -sSL '{url}'\n"
        f"\n"
        f"About: {summary}\n"
    )

    click.echo("\n" + clipboard_text)
    if not no_copy and _copy(clipboard_text):
        click.echo(f"{click.style('✔ copied to clipboard', fg='green')}", err=True)
    elif not no_copy:
        click.echo(click.style("(pbcopy unavailable — text above is the share)", fg="yellow"), err=True)


@share.command("list")
def list_cmd() -> None:
    """List your active share links."""
    with httpx.Client(base_url=_share_url(), timeout=15, headers=_share_headers()) as client:
        resp = client.get("/share")
        resp.raise_for_status()
        shares = resp.json()

    if not shares:
        click.echo("(no active shares)")
        return
    for s in shares:
        once = " [once]" if s.get("once") else ""
        click.echo(f"{s['token']}{once}  expires {s['expires_at']}  {s.get('title') or ''}")
        click.echo(f"  {s['url']}")


@share.command("revoke")
@click.argument("token")
def revoke_cmd(token: str) -> None:
    """Revoke a share link early."""
    with httpx.Client(base_url=_share_url(), timeout=15, headers=_share_headers()) as client:
        resp = client.delete(f"/share/{token}")
        if resp.status_code == 404:
            raise click.ClickException("Not found or not owned by you")
        resp.raise_for_status()
    click.echo(f"Revoked {token}")
