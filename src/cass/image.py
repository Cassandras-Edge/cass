"""ChatGPT-subscription-backed image generation.

Routes through chatgpt.com/backend-api/codex/responses using the Codex OAuth
credentials at ~/.codex/auth.json. This is the same undocumented endpoint the
Codex CLI's built-in image_gen tool uses — we piggyback on your ChatGPT Plus/
Pro subscription instead of requiring a separate OPENAI_API_KEY.

Pattern mirrored from samSaffron/term-llm's ChatGPT image provider (MIT).
"""

from __future__ import annotations

import base64
import json
import time
import sys
from dataclasses import dataclass
from datetime import datetime
from pathlib import Path
from typing import Iterator

import click
import httpx


CODEX_AUTH_PATH = Path.home() / ".codex" / "auth.json"
RESPONSES_URL = "https://chatgpt.com/backend-api/codex/responses"
TOKEN_URL = "https://auth.openai.com/oauth/token"
CODEX_CLIENT_ID = "app_EMoamEEZ73f0CkXaXp7hrann"
DEFAULT_MODEL = "gpt-5.4-mini"
EXP_BUFFER_SECS = 300  # refresh 5 min before JWT exp


class ImageGenError(Exception):
    pass


@dataclass
class ImageResult:
    data: bytes
    revised_prompt: str = ""
    mime_type: str = "image/png"


def _decode_jwt_exp(access_token: str) -> int:
    """Return the `exp` claim from a JWT (no signature verification)."""
    try:
        parts = access_token.split(".")
        if len(parts) < 2:
            raise ValueError("not a JWT")
        payload_b64 = parts[1]
        padding = (-len(payload_b64)) % 4
        payload = json.loads(
            base64.urlsafe_b64decode(payload_b64 + "=" * padding).decode()
        )
        return int(payload["exp"])
    except Exception as e:
        raise ImageGenError(f"failed to decode access_token: {e}")


def _load_creds() -> dict:
    if not CODEX_AUTH_PATH.exists():
        raise ImageGenError(
            f"No Codex login found at {CODEX_AUTH_PATH}. "
            "Install and log in to Codex first: `codex login` "
            "(ChatGPT Plus/Pro subscription required)."
        )
    with CODEX_AUTH_PATH.open() as f:
        auth = json.load(f)
    if auth.get("auth_mode") != "chatgpt":
        raise ImageGenError(
            f"Codex auth_mode is {auth.get('auth_mode')!r}; need 'chatgpt'. "
            "Run `codex logout && codex login` to switch to ChatGPT subscription mode."
        )
    tokens = auth.get("tokens") or {}
    if not tokens.get("access_token") or not tokens.get("refresh_token"):
        raise ImageGenError(
            f"Malformed {CODEX_AUTH_PATH}: tokens.access_token or refresh_token missing."
        )
    return auth


def _save_creds(auth: dict) -> None:
    auth["last_refresh"] = time.strftime("%Y-%m-%dT%H:%M:%S.000Z", time.gmtime())
    CODEX_AUTH_PATH.write_text(json.dumps(auth, indent=2))


def _refresh_if_needed(auth: dict) -> dict:
    tokens = auth["tokens"]
    exp = _decode_jwt_exp(tokens["access_token"])
    if int(time.time()) < exp - EXP_BUFFER_SECS:
        return auth

    with httpx.Client(timeout=30.0) as client:
        resp = client.post(
            TOKEN_URL,
            data={
                "grant_type": "refresh_token",
                "client_id": CODEX_CLIENT_ID,
                "refresh_token": tokens["refresh_token"],
                "scope": "openid profile email offline_access",
            },
            headers={"Content-Type": "application/x-www-form-urlencoded"},
        )
    if resp.status_code != 200:
        raise ImageGenError(
            f"Token refresh failed: HTTP {resp.status_code} {resp.text[:300]}"
        )
    body = resp.json()
    tokens["access_token"] = body["access_token"]
    if body.get("refresh_token"):
        tokens["refresh_token"] = body["refresh_token"]
    if body.get("id_token"):
        tokens["id_token"] = body["id_token"]
    _save_creds(auth)
    return auth


def _iter_sse(resp: httpx.Response) -> Iterator[tuple[str, str]]:
    """Yield (event_name, data_string) from an SSE response."""
    event = "message"
    data_lines: list[str] = []
    for raw in resp.iter_lines():
        if raw == "":
            if data_lines:
                yield event, "\n".join(data_lines)
                event = "message"
                data_lines = []
            continue
        if raw.startswith(":"):
            continue
        if raw.startswith("event:"):
            event = raw[6:].strip()
        elif raw.startswith("data:"):
            data_lines.append(raw[5:].lstrip())


def _decorate_prompt(prompt: str, size: str = "", aspect: str = "") -> str:
    """Append plain-English size/aspect hints — the built-in image_gen tool
    exposes no size/aspect parameters, so we steer via prompt text instead.
    """
    hints: list[str] = []
    if size:
        s = size.upper()
        res = {"1K": "1024x1024", "2K": "2048x2048", "4K": "4096x4096"}.get(s)
        if res:
            hints.append(f"Target resolution: approximately {res} pixels ({s}).")
    if aspect:
        hints.append(f"Aspect ratio: {aspect}.")
    if not hints:
        return prompt
    return prompt + "\n\n" + " ".join(hints)


def generate(
    prompt: str,
    *,
    input_image: bytes | None = None,
    input_mime: str = "image/png",
    size: str = "",
    aspect: str = "",
    quality: str = "",
    model: str = DEFAULT_MODEL,
    timeout: float = 180.0,
) -> ImageResult:
    """Generate (or edit) an image.

    With input_image, runs edit mode — the model receives the image plus prompt
    and returns a modified version. Without input_image, runs pure generation.
    """
    auth = _refresh_if_needed(_load_creds())
    tokens = auth["tokens"]
    account_id = tokens.get("account_id") or auth.get("account_id") or ""

    content: list[dict] = [
        {"type": "input_text", "text": _decorate_prompt(prompt, size, aspect)}
    ]
    if input_image is not None:
        data_url = f"data:{input_mime};base64,{base64.b64encode(input_image).decode()}"
        content.append({"type": "input_image", "image_url": data_url})
        instructions = (
            "You are an image editing assistant. The user provides an input "
            "image and instructions to modify it. Call the image_generation "
            "tool to produce exactly one edited image."
        )
    else:
        instructions = (
            "You are an image generation assistant. Generate exactly one image "
            "matching the user's prompt by calling the image_generation tool."
        )

    image_tool: dict = {"type": "image_generation", "output_format": "png"}
    if quality:
        image_tool["quality"] = quality
    body = {
        "model": model,
        "instructions": instructions,
        "input": [{"type": "message", "role": "user", "content": content}],
        "tools": [image_tool],
        "tool_choice": {"type": "image_generation"},
        "store": False,
        "stream": True,
    }
    headers = {
        "Authorization": f"Bearer {tokens['access_token']}",
        "ChatGPT-Account-ID": account_id,
        "OpenAI-Beta": "responses=experimental",
        "originator": "cass",
        "Content-Type": "application/json",
        "Accept": "text/event-stream",
    }

    with httpx.Client(timeout=timeout) as client:
        with client.stream("POST", RESPONSES_URL, json=body, headers=headers) as resp:
            if resp.status_code != 200:
                try:
                    err_body = b"".join(resp.iter_raw()).decode(errors="replace")
                except Exception:
                    err_body = ""
                raise ImageGenError(
                    f"Responses API HTTP {resp.status_code}: {err_body[:500]}"
                )

            for event_name, data in _iter_sse(resp):
                if event_name != "response.output_item.done":
                    continue
                try:
                    obj = json.loads(data)
                except json.JSONDecodeError:
                    continue
                item = obj.get("item") or {}
                if item.get("type") != "image_generation_call":
                    continue
                b64 = item.get("result") or ""
                if not b64:
                    continue
                try:
                    png_bytes = base64.b64decode(b64)
                except Exception as e:
                    raise ImageGenError(f"decode image result: {e}")
                return ImageResult(
                    data=png_bytes,
                    revised_prompt=item.get("revised_prompt") or "",
                    mime_type="image/png",
                )

    raise ImageGenError("stream ended without an image_generation_call result")


# ---------- Click CLI ----------


def _default_out_path() -> Path:
    stamp = datetime.now().strftime("%Y%m%d-%H%M%S")
    return Path.home() / "Downloads" / f"cass-img-{stamp}.png"


@click.command()
@click.argument("prompt")
@click.option("--out", "-o", type=click.Path(dir_okay=False, path_type=Path),
              help="Output path (default: ~/Downloads/cass-img-<ts>.png)")
@click.option("--edit", "-e", "edit_path",
              type=click.Path(exists=True, dir_okay=False, path_type=Path),
              help="Input image to edit (runs edit mode)")
@click.option("--aspect", "-a", default="",
              help="Aspect ratio hint (e.g. 16:9, 1:1, 3:4)")
@click.option("--size", "-s", default="",
              type=click.Choice(["", "1K", "2K", "4K"], case_sensitive=False),
              help="Target resolution hint")
@click.option("--model", "-m", default=DEFAULT_MODEL, show_default=True,
              help="Agent model (renderer is server-picked, currently gpt-image-2)")
@click.option("--fast", "-f", is_flag=True,
              help="Shorthand for --quality low — faster render, lower fidelity")
@click.option("--quality", "-q", default="",
              type=click.Choice(["", "low", "medium", "high", "auto"],
                                case_sensitive=False),
              help="Render quality (overrides --fast)")
@click.option("--open/--no-open", "open_after", default=True,
              help="Open the generated image after saving (default: yes)")
@click.pass_context
def image(ctx: click.Context, prompt: str, out: Path | None,
          edit_path: Path | None, aspect: str, size: str, model: str,
          fast: bool, quality: str, open_after: bool) -> None:
    """Generate or edit an image via your ChatGPT Plus/Pro subscription.

    Uses the same undocumented endpoint Codex's built-in image_gen tool hits
    (chatgpt.com/backend-api/codex/responses). Requires `codex login` with
    a ChatGPT subscription — auth is reused from ~/.codex/auth.json.
    """
    input_bytes = edit_path.read_bytes() if edit_path else None
    input_mime = "image/png"
    if edit_path:
        suffix = edit_path.suffix.lower()
        input_mime = {
            ".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
            ".webp": "image/webp", ".gif": "image/gif",
        }.get(suffix, "image/png")

    out_path = out or _default_out_path()
    out_path.parent.mkdir(parents=True, exist_ok=True)

    resolved_quality = (quality or ("low" if fast else "")).lower()

    click.echo(f"Generating{'  (edit)' if edit_path else ''}...", err=True)
    try:
        result = generate(
            prompt,
            input_image=input_bytes,
            input_mime=input_mime,
            size=size,
            aspect=aspect,
            quality=resolved_quality,
            model=model,
        )
    except ImageGenError as e:
        click.echo(f"Error: {e}", err=True)
        ctx.exit(1)

    out_path.write_bytes(result.data)
    click.echo(str(out_path))
    if result.revised_prompt:
        click.echo(f"(revised prompt: {result.revised_prompt})", err=True)

    if open_after:
        _open_file(out_path)


def _open_file(path: Path) -> None:
    import subprocess
    try:
        if sys.platform == "darwin":
            subprocess.run(["open", str(path)], check=False)
        elif sys.platform.startswith("linux"):
            subprocess.run(["xdg-open", str(path)], check=False)
        elif sys.platform == "win32":
            subprocess.run(["cmd", "/c", "start", "", str(path)], check=False)
    except Exception:
        pass


