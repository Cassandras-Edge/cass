"""Stdio MCP server exposing cass image generation as tools.

Runs as a local subprocess of the calling agent (Claude Code, Codex, etc.).
The agent gets back both a local file path (TextContent) and the image itself
(ImageContent), so it can save, reference, and visually inspect the result.
"""

from __future__ import annotations

from datetime import datetime
from pathlib import Path

from fastmcp import FastMCP
from fastmcp.utilities.types import Image

from cass.image import DEFAULT_MODEL, ImageGenError, generate


mcp = FastMCP("cass-image")


def _resolve_out(out_path: str | None) -> Path:
    if out_path:
        p = Path(out_path).expanduser()
    else:
        stamp = datetime.now().strftime("%Y%m%d-%H%M%S")
        p = Path.home() / "Downloads" / f"cass-img-{stamp}.png"
    p.parent.mkdir(parents=True, exist_ok=True)
    return p


@mcp.tool
def generate_image(
    prompt: str,
    out_path: str = "",
    aspect: str = "",
    size: str = "",
    model: str = DEFAULT_MODEL,
) -> list:
    """Generate an image from a text prompt.

    Uses the caller's ChatGPT Plus/Pro subscription via Codex OAuth credentials
    at ~/.codex/auth.json. Currently routes to gpt-image-2 server-side.

    Args:
        prompt: What to generate.
        out_path: Where to save the PNG. Defaults to ~/Downloads/cass-img-<ts>.png.
        aspect: Aspect ratio hint (e.g. "16:9", "1:1"). Injected into prompt.
        size: Resolution hint: "1K", "2K", or "4K". Injected into prompt.
        model: Agent model routing the tool call.

    Returns:
        TextContent with the saved file path + ImageContent with the PNG.
    """
    out = _resolve_out(out_path)
    try:
        result = generate(prompt, size=size, aspect=aspect, model=model)
    except ImageGenError as e:
        return [f"Image generation failed: {e}"]
    out.write_bytes(result.data)
    text = f"Saved to {out}"
    if result.revised_prompt:
        text += f"\nRevised prompt: {result.revised_prompt}"
    return [text, Image(data=result.data, format="png")]


@mcp.tool
def edit_image(
    prompt: str,
    input_path: str,
    out_path: str = "",
    aspect: str = "",
    size: str = "",
    model: str = DEFAULT_MODEL,
) -> list:
    """Edit an existing image given a natural-language instruction.

    Args:
        prompt: What to change.
        input_path: Path to the source image (png/jpg/webp/gif).
        out_path: Where to save the edited PNG. Defaults to <input>-edited-<ts>.png
            next to the source.
        aspect: Aspect ratio hint. Injected into prompt.
        size: Resolution hint: "1K", "2K", or "4K". Injected into prompt.
        model: Agent model routing the tool call.

    Returns:
        TextContent with the saved file path + ImageContent with the PNG.
    """
    src = Path(input_path).expanduser()
    if not src.exists():
        return [f"Input image not found: {src}"]
    suffix = src.suffix.lower()
    mime = {
        ".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
        ".webp": "image/webp", ".gif": "image/gif",
    }.get(suffix, "image/png")

    if out_path:
        out = Path(out_path).expanduser()
    else:
        stamp = datetime.now().strftime("%Y%m%d-%H%M%S")
        out = src.with_name(f"{src.stem}-edited-{stamp}.png")
    out.parent.mkdir(parents=True, exist_ok=True)

    try:
        result = generate(
            prompt,
            input_image=src.read_bytes(),
            input_mime=mime,
            size=size,
            aspect=aspect,
            model=model,
        )
    except ImageGenError as e:
        return [f"Image edit failed: {e}"]
    out.write_bytes(result.data)
    text = f"Saved edited image to {out}"
    if result.revised_prompt:
        text += f"\nRevised prompt: {result.revised_prompt}"
    return [text, Image(data=result.data, format="png")]


def run_mcp_server() -> None:
    """Entry point for `cass image mcp`."""
    mcp.run()  # stdio transport by default
