"""Self-update — download latest cass binary from GitHub releases."""

from __future__ import annotations

import os
import platform
import shutil
import stat
import sys
import tempfile

import click
import httpx

REPO = "Cassandras-Edge/cass"
CURRENT_VERSION = "0.6.1"


def _detect_target() -> str:
    system = platform.system().lower()
    machine = platform.machine().lower()

    if machine in ("x86_64", "amd64"):
        arch = "amd64"
    elif machine in ("arm64", "aarch64"):
        arch = "arm64"
    else:
        raise click.ClickException(f"Unsupported architecture: {machine}")

    if system == "darwin":
        return f"darwin-{arch}"
    elif system == "linux":
        return f"linux-{arch}"
    elif system == "windows":
        return f"windows-{arch}"
    else:
        raise click.ClickException(f"Unsupported OS: {system}")


def _get_latest_release() -> dict:
    resp = httpx.get(f"https://api.github.com/repos/{REPO}/releases/latest", timeout=15)
    resp.raise_for_status()
    return resp.json()


@click.command()
@click.option("--check", is_flag=True, help="Check for updates without installing.")
def update(check: bool) -> None:
    """Update cass to the latest version."""
    click.echo(f"Current version: {CURRENT_VERSION}")

    try:
        release = _get_latest_release()
    except httpx.HTTPError as e:
        raise click.ClickException(f"Failed to check for updates: {e}") from e

    latest = release["tag_name"].lstrip("v")
    click.echo(f"Latest version:  {latest}")

    if latest == CURRENT_VERSION:
        click.echo("Already up to date.")
        return

    if check:
        click.echo(f"Update available: {CURRENT_VERSION} → {latest}")
        return

    target = _detect_target()
    asset_name = f"cass-{target}"
    if "windows" in target:
        asset_name += ".exe"

    # Find the download URL
    url = None
    for asset in release.get("assets", []):
        if asset["name"] == asset_name:
            url = asset["browser_download_url"]
            break

    if not url:
        raise click.ClickException(f"No binary found for {target} in release {latest}")

    click.echo(f"Downloading {asset_name}...")

    # Download to temp file
    with tempfile.NamedTemporaryFile(delete=False, suffix=".tmp") as tmp:
        tmp_path = tmp.name
        with httpx.stream("GET", url, follow_redirects=True, timeout=60) as resp:
            resp.raise_for_status()
            for chunk in resp.iter_bytes():
                tmp.write(chunk)

    # Replace current binary
    current_bin = shutil.which("cass") or sys.executable
    # If running from a uv tool install, update the plugin data dir instead
    plugin_data = os.environ.get("CLAUDE_PLUGIN_DATA")
    if plugin_data:
        target_path = os.path.join(plugin_data, "bin", "cass")
    else:
        target_path = current_bin

    try:
        os.replace(tmp_path, target_path)
        os.chmod(target_path, os.stat(target_path).st_mode | stat.S_IEXEC)
    except OSError:
        # Can't replace in-place (Windows locks, etc.) — try backup approach
        backup = target_path + ".bak"
        try:
            os.replace(target_path, backup)
            os.replace(tmp_path, target_path)
            os.chmod(target_path, os.stat(target_path).st_mode | stat.S_IEXEC)
            os.unlink(backup)
        except OSError as e:
            raise click.ClickException(f"Failed to replace binary: {e}") from e

    click.echo(f"Updated: {CURRENT_VERSION} → {latest}")


def auto_update_check() -> None:
    """Silent background update check — downloads new version if available.

    Called automatically on every cass invocation (rate-limited by caller).
    Swallows all errors — must never break the user's command.
    """
    try:
        release = _get_latest_release()
        latest = release["tag_name"].lstrip("v")
        if latest == CURRENT_VERSION:
            return

        target = _detect_target()
        asset_name = f"cass-{target}"
        if "windows" in target:
            asset_name += ".exe"

        url = None
        for asset in release.get("assets", []):
            if asset["name"] == asset_name:
                url = asset["browser_download_url"]
                break
        if not url:
            return

        click.echo(f"Updating cass {CURRENT_VERSION} → {latest}...", err=True)

        import tempfile  # noqa: PLC0415

        with tempfile.NamedTemporaryFile(delete=False, suffix=".tmp") as tmp:
            tmp_path = tmp.name
            with httpx.stream("GET", url, follow_redirects=True, timeout=60) as resp:
                resp.raise_for_status()
                for chunk in resp.iter_bytes():
                    tmp.write(chunk)

        current_bin = shutil.which("cass") or sys.executable
        plugin_data = os.environ.get("CLAUDE_PLUGIN_DATA")
        if plugin_data:
            target_path = os.path.join(plugin_data, "bin", "cass")
        else:
            target_path = current_bin

        os.replace(tmp_path, target_path)
        os.chmod(target_path, os.stat(target_path).st_mode | stat.S_IEXEC)
        click.echo(f"Updated to {latest}.", err=True)
    except Exception:  # noqa: BLE001
        pass  # never break the user's command
