"""Self-update — download cass binary from GitHub releases."""

from __future__ import annotations

import os
import platform
import shutil
import stat
import subprocess
import sys
import tempfile
from importlib.metadata import PackageNotFoundError, version as _pkg_version

import click
import httpx

REPO = "Cassandras-Edge/cass"

# Read from package metadata so releases don't drift from pyproject.toml.
# PyInstaller onefile builds include dist-info when the wheel is installed
# before the build (release.yml does `pip install -e .` first).
try:
    CURRENT_VERSION = _pkg_version("cass")
except PackageNotFoundError:
    CURRENT_VERSION = "0.0.0-dev"


def _is_scoop_managed() -> bool:
    """True if the running cass binary lives inside a scoop apps dir.

    Scoop installs to `%USERPROFILE%\\scoop\\apps\\<app>\\current\\<bin>.exe`
    (or `%SCOOP%\\apps\\...`). If cass is there, users must update via
    `scoop update cass` so scoop's manifest ledger stays in sync.
    """
    if platform.system().lower() != "windows":
        return False
    bin_path = shutil.which("cass") or sys.executable
    norm = os.path.normpath(bin_path).lower()
    return os.sep + "scoop" + os.sep + "apps" + os.sep + "cass" + os.sep in norm


def _run_scoop_update(silent: bool = False) -> bool:
    """Run `scoop update cass`. Returns True on success."""
    scoop = shutil.which("scoop")
    if not scoop:
        if not silent:
            click.echo(
                "cass is installed via scoop but the scoop CLI is not on PATH.\n"
                "Open a new PowerShell session or reinstall scoop, then re-run.",
                err=True,
            )
        return False
    try:
        # `scoop` on Windows is a shim that needs the shell; use shell=True only
        # when invoking via cmd. subprocess.run with the shim path works directly.
        result = subprocess.run(
            [scoop, "update", "cass"],
            capture_output=silent,
            text=True,
            timeout=300,
        )
        return result.returncode == 0
    except (subprocess.TimeoutExpired, OSError):
        return False


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


def _get_release_by_tag(tag: str) -> dict:
    resp = httpx.get(f"https://api.github.com/repos/{REPO}/releases/tags/{tag}", timeout=15)
    resp.raise_for_status()
    return resp.json()


def _resolve_release(target: str) -> dict:
    """Resolve 'latest' | 'stable' | '<version>' | 'v<version>' to a GitHub release."""
    if target in ("latest", "stable"):
        return _get_latest_release()
    tag = target if target.startswith("v") else f"v{target}"
    return _get_release_by_tag(tag)


def _install_release(release: dict) -> str:
    """Download release binary for the current platform and replace the
    on-disk cass. Returns the installed version string."""
    version = release["tag_name"].lstrip("v")
    target = _detect_target()
    asset_name = f"cass-{target}"
    if "windows" in target:
        asset_name += ".exe"

    url = next(
        (a["browser_download_url"] for a in release.get("assets", []) if a["name"] == asset_name),
        None,
    )
    if not url:
        raise click.ClickException(f"No binary found for {target} in release {version}")

    click.echo(f"Downloading {asset_name}...")

    with tempfile.NamedTemporaryFile(delete=False, suffix=".tmp") as tmp:
        tmp_path = tmp.name
        with httpx.stream("GET", url, follow_redirects=True, timeout=60) as resp:
            resp.raise_for_status()
            for chunk in resp.iter_bytes():
                tmp.write(chunk)

    current_bin = shutil.which("cass") or sys.executable
    plugin_data = os.environ.get("CLAUDE_PLUGIN_DATA")
    target_path = os.path.join(plugin_data, "bin", "cass") if plugin_data else current_bin

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

    return version


@click.command()
@click.argument("target", default="latest", required=False)
@click.option("--force", is_flag=True, help="Reinstall even if already at target version.")
def install(target: str, force: bool) -> None:
    """Install cass — latest, stable, or a specific version.

    Mirrors `claude install [target]` UX. Examples:

        cass install              # install latest
        cass install latest       # same
        cass install 0.6.8        # install v0.6.8
        cass install v0.6.8       # same
    """
    click.echo(f"Current version: {CURRENT_VERSION}")

    if _is_scoop_managed():
        if target not in ("latest", "stable"):
            raise click.ClickException(
                "Pinning a specific version isn't supported for scoop-managed "
                "installs. Run `scoop update cass` for the latest, or reinstall."
            )
        click.echo("scoop-managed install detected — delegating to `scoop update cass`.")
        if not _run_scoop_update():
            raise click.ClickException("`scoop update cass` failed.")
        return

    try:
        release = _resolve_release(target)
    except httpx.HTTPError as e:
        raise click.ClickException(f"Failed to resolve release '{target}': {e}") from e

    desired = release["tag_name"].lstrip("v")
    click.echo(f"Target version:  {desired}")

    if desired == CURRENT_VERSION and not force:
        click.echo("Already at target version. Use --force to reinstall.")
        return

    installed = _install_release(release)
    click.echo(f"Installed: {CURRENT_VERSION} → {installed}")


@click.command()
@click.option("--check", is_flag=True, help="Only report what would be updated, don't install.")
@click.option("--binary-only", is_flag=True, help="Update the cass binary only; skip plugins/patched-cli/keys.")
@click.option(
    "--client",
    type=click.Choice(["auto", "claude", "codex", "both"]),
    default="auto",
    show_default=True,
    help="Which client integrations to update after the binary update.",
)
def update(check: bool, binary_only: bool, client: str) -> None:
    """Update everything — the cass binary plus configured Claude/Codex integrations.
    """
    click.echo(f"Current version: {CURRENT_VERSION}")
    scoop_managed = _is_scoop_managed()

    try:
        release = _get_latest_release()
    except httpx.HTTPError as e:
        raise click.ClickException(f"Failed to check for updates: {e}") from e

    latest = release["tag_name"].lstrip("v")
    click.echo(f"Latest version:  {latest}")

    if check:
        if latest == CURRENT_VERSION:
            click.echo("cass is up to date. Plugins/patched CLI not checked in --check mode.")
        else:
            click.echo(f"cass update available: {CURRENT_VERSION} → {latest}")
        return

    if latest != CURRENT_VERSION:
        if scoop_managed:
            click.echo("scoop-managed install detected — delegating to `scoop update cass`.")
            if not _run_scoop_update():
                raise click.ClickException("`scoop update cass` failed.")
            click.echo(f"Updated cass via scoop to {latest}.")
        else:
            installed_version = _install_release(release)
            click.echo(f"Updated cass: {CURRENT_VERSION} → {installed_version}")
    else:
        click.echo("cass binary is up to date.")

    if binary_only:
        return

    # Sync the rest of the stack. Deferred import avoids a circular between
    # cli.py → update.py (at --version time) and setup.py → auth.py.
    from cass.setup import sync_platform  # noqa: PLC0415
    click.echo("")
    sync_platform(install_missing=False, client=client)
    click.echo("")
    click.echo("Update complete. Restart your configured client(s) to pick up the changes.")


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
        if _is_scoop_managed():
            click.echo(
                f"cass {latest} is available. Run `scoop update cass` to upgrade.",
                err=True,
            )
            return
        click.echo(f"Updating cass {CURRENT_VERSION} → {latest}...", err=True)
        _install_release(release)
        click.echo(f"Updated to {latest}.", err=True)
    except Exception:  # noqa: BLE001
        pass  # never break the user's command
