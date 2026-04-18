"""Install and manage the patched Claude Code CLI used by marketplace plugins.

Three install paths:

1. **Prebuilt (default)**: download platform-specific artifact from
   cassandra-cc-patches GitHub Releases. No local toolchain required.
2. **Legacy (2.1.112, --local)**: npm ships a plain `cli.js`. Patch in place.
3. **Repack (2.1.113+, --local)**: npm ships a native binary. Extract-patch-
   recompile via `cassandra-cc-patches/scripts/repack-binary.js`.

Marketplace plugins check `~/.local/bin/claude-patched` directly — no env var.
"""

from __future__ import annotations

import platform
import shutil
import subprocess
from pathlib import Path

import click


CLI_VERSION = "2.1.113"
INSTALL_PREFIX = Path.home() / ".local" / "share" / "claude-patched"
BIN_PATH = Path.home() / ".local" / "bin" / "claude-patched"
CLI_JS_REL = Path("node_modules") / "@anthropic-ai" / "claude-code" / "cli.js"

CC_PATCHES_REPO = "Cassandras-Edge/cassandra-cc-patches"
CC_PATCHES_CANDIDATES = [
    Path.home() / "cassandra-stack" / "cassandra-cc-patches",
    Path.home() / "src" / "cassandra-cc-patches",
    Path("/opt/cassandra-cc-patches"),
]


def _version_tuple(v: str) -> tuple[int, ...]:
    return tuple(int(p) for p in v.split(".") if p.isdigit())


def _uses_repack(version: str) -> bool:
    """2.1.113+ ships native-only — needs repack pipeline."""
    return _version_tuple(version) >= (2, 1, 113)


@click.group("patched-cli")
def patched_cli() -> None:
    """Manage the patched Claude Code CLI at ~/.local/bin/claude-patched.

    Used by marketplace plugins (e.g. stopgate) when they need `claude --bare`
    with OAuth. The install path differs by version:
    - 2.1.112: patched cli.js run via node shebang
    - 2.1.113+: repacked standalone bun binary (extract, patch, recompile)
    """


@patched_cli.command()
@click.option("--version", default=CLI_VERSION, show_default=True, help="claude-code npm version (used by --local only; prebuilt uses release tag)")
@click.option("--local", is_flag=True, help="Build locally from cassandra-cc-patches instead of downloading prebuilt.")
@click.option("--release", "release_tag", default=None, help="Specific cc-patches release tag (default: latest).")
def install(version: str, local: bool, release_tag: str | None) -> None:
    """Install the patched Claude Code CLI to ~/.local/bin/claude-patched."""
    require_supported_host()
    BIN_PATH.parent.mkdir(parents=True, exist_ok=True)

    if local:
        _install_local(version)
    else:
        _install_prebuilt(release_tag)

    click.echo("")
    click.echo(f"Installed: {BIN_PATH}")
    _print_version()


def _install_prebuilt(release_tag: str | None) -> None:
    """Download platform-specific prebuilt from cassandra-cc-patches GitHub Releases."""
    if shutil.which("gh") is None:
        raise click.ClickException(
            "gh CLI not found. Install: brew install gh && gh auth login\n"
            "Or use --local to build from source."
        )

    asset = f"claude-patched-{_host_target()}"
    tag_args = ["--tag", release_tag] if release_tag else []
    click.echo(f"Downloading {asset} from {CC_PATCHES_REPO}{' @ ' + release_tag if release_tag else ' (latest)'}...")

    if BIN_PATH.is_symlink() or BIN_PATH.exists():
        BIN_PATH.unlink()
    subprocess.run(
        ["gh", "release", "download", *tag_args,
         "--repo", CC_PATCHES_REPO,
         "--pattern", asset,
         "--output", str(BIN_PATH)],
        check=True,
    )
    BIN_PATH.chmod(0o755)
    _smoke_test_any()


def _install_local(version: str) -> None:
    """Build locally from cassandra-cc-patches (current behavior)."""
    if shutil.which("npm") is None:
        raise click.ClickException("npm not found. Install Node.js first.")
    if shutil.which("node") is None:
        raise click.ClickException("node not found. Install Node.js first.")

    cc_patches = _find_cc_patches()

    click.echo(f"Installing @anthropic-ai/claude-code@{version} -> {INSTALL_PREFIX}")
    INSTALL_PREFIX.mkdir(parents=True, exist_ok=True)
    if not (INSTALL_PREFIX / "package.json").exists():
        subprocess.run(
            ["npm", "init", "-y"],
            cwd=INSTALL_PREFIX, check=True, capture_output=True,
        )
    subprocess.run(
        ["npm", "install", f"@anthropic-ai/claude-code@{version}"],
        cwd=INSTALL_PREFIX, check=True,
    )

    if _uses_repack(version):
        _install_repack(cc_patches, version)
    else:
        _install_legacy(cc_patches, version)

    _smoke_test(version)


def require_supported_host() -> None:
    """Raise if we're running on native Windows. WSL reports as Linux and is fine."""
    if platform.system().lower() == "windows":
        raise click.ClickException(
            "Native Windows is not supported.\n"
            "Run cass inside WSL — the linux-x64 build works there with zero changes.\n"
            "See: https://learn.microsoft.com/windows/wsl/install"
        )


def _host_target() -> str:
    """Map host platform/arch to release artifact suffix (darwin-arm64, linux-x64, ...)."""
    require_supported_host()
    system = platform.system().lower()
    arch = {"arm64": "arm64", "aarch64": "arm64", "x86_64": "x64", "amd64": "x64"}.get(
        platform.machine().lower(), platform.machine().lower()
    )
    if system not in {"darwin", "linux"}:
        raise click.ClickException(f"Unsupported host platform: {system}")
    return f"{system}-{arch}"


def _smoke_test_any() -> None:
    """Smoke test without a version expectation (prebuilt path)."""
    result = subprocess.run(
        [str(BIN_PATH), "--version"],
        capture_output=True, text=True, timeout=15,
    )
    if result.returncode != 0 or not result.stdout.strip():
        raise click.ClickException(
            f"Smoke test failed: {result.stdout.strip() or '(empty)'}"
        )


def _print_version() -> None:
    try:
        r = subprocess.run([str(BIN_PATH), "--version"], capture_output=True, text=True, timeout=10)
        click.echo(f"Version:   {r.stdout.strip() or '(empty)'}")
    except Exception:
        pass


def _install_legacy(cc_patches: Path, version: str) -> None:
    """Patch cli.js in place, symlink to it (2.1.112 and older)."""
    cli_js = INSTALL_PREFIX / CLI_JS_REL
    if not cli_js.exists():
        raise click.ClickException(f"cli.js not found after npm install: {cli_js}")

    orig = cli_js.with_suffix(cli_js.suffix + ".orig")
    if orig.exists():
        shutil.copy2(orig, cli_js)
    else:
        shutil.copy2(cli_js, orig)

    click.echo(f"Applying patches from {cc_patches} (js-only)")
    subprocess.run(
        ["node", "scripts/patch-all.js", "--binary", str(cli_js), "--js-only"],
        cwd=cc_patches, check=True,
    )

    patched_out = cc_patches / "dist" / "cli-patched.js"
    if not patched_out.exists():
        raise click.ClickException(f"Patcher did not produce {patched_out}")
    shutil.copy2(patched_out, cli_js)
    cli_js.chmod(0o755)

    BIN_PATH.parent.mkdir(parents=True, exist_ok=True)
    if BIN_PATH.is_symlink() or BIN_PATH.exists():
        BIN_PATH.unlink()
    BIN_PATH.symlink_to(cli_js)


def _install_repack(cc_patches: Path, version: str) -> None:
    """Run the full extract-patch-recompile pipeline (2.1.113+)."""
    if shutil.which("bun") is None:
        raise click.ClickException(
            "bun not found — required for the 2.1.113+ repack pipeline.\n"
            "Install: curl -fsSL https://bun.com/install | bash"
        )

    native = _find_native_binary()
    click.echo(f"Running repack pipeline on {native}")

    BIN_PATH.parent.mkdir(parents=True, exist_ok=True)
    # Replace any prior install (symlink or file)
    if BIN_PATH.is_symlink() or BIN_PATH.exists():
        BIN_PATH.unlink()

    subprocess.run(
        [
            "bun", "run", "scripts/repack-binary.js",
            "--binary", str(native),
            "--outfile", str(BIN_PATH),
        ],
        cwd=cc_patches, check=True,
    )


def _find_native_binary() -> Path:
    """Locate the platform-specific native binary under the isolated npm install."""
    import platform
    system = platform.system().lower()
    arch = {"arm64": "arm64", "aarch64": "arm64", "x86_64": "x64", "amd64": "x64"}.get(
        platform.machine().lower(), platform.machine().lower()
    )
    platform_pkg = f"claude-code-{system}-{arch}"
    candidate = INSTALL_PREFIX / "node_modules" / "@anthropic-ai" / platform_pkg / "claude"
    if not candidate.exists():
        # Fall back to scanning for any platform package
        base = INSTALL_PREFIX / "node_modules" / "@anthropic-ai"
        for sub in base.iterdir() if base.exists() else []:
            cand = sub / "claude"
            if cand.exists() and cand.is_file():
                return cand
        raise click.ClickException(f"Native claude binary not found. Expected: {candidate}")
    return candidate


def _smoke_test(expected_version: str) -> None:
    result = subprocess.run(
        [str(BIN_PATH), "--version"],
        capture_output=True, text=True, timeout=15,
    )
    if result.returncode != 0 or expected_version not in result.stdout:
        raise click.ClickException(
            f"Smoke test failed. Expected {expected_version}, got: {result.stdout.strip() or '(empty)'}"
        )


@patched_cli.command()
def status() -> None:
    """Show patched CLI installation status."""
    if not (BIN_PATH.is_symlink() or BIN_PATH.exists()):
        click.echo(f"Not installed. Run: cass patched-cli install")
        raise SystemExit(1)

    kind = "symlink" if BIN_PATH.is_symlink() else "binary"
    click.echo(f"Path:    {BIN_PATH} ({kind})")
    if BIN_PATH.is_symlink():
        click.echo(f"Target:  {BIN_PATH.resolve()}")
    try:
        result = subprocess.run(
            [str(BIN_PATH), "--version"],
            capture_output=True, text=True, timeout=10,
        )
        click.echo(f"Version: {result.stdout.strip() or '(empty)'}")
    except Exception as e:
        click.echo(f"Version: (failed: {e})")


@patched_cli.command()
def restore() -> None:
    """Remove the patched CLI install."""
    removed = False
    if BIN_PATH.is_symlink() or BIN_PATH.exists():
        BIN_PATH.unlink()
        click.echo(f"Removed {BIN_PATH}")
        removed = True
    if INSTALL_PREFIX.exists():
        shutil.rmtree(INSTALL_PREFIX)
        click.echo(f"Removed {INSTALL_PREFIX}")
        removed = True
    if not removed:
        click.echo("Nothing to remove.")


def _find_cc_patches() -> Path:
    for p in CC_PATCHES_CANDIDATES:
        if (p / "scripts" / "patch-all.js").exists():
            return p
    candidates = "\n  ".join(str(p) for p in CC_PATCHES_CANDIDATES)
    raise click.ClickException(
        "cassandra-cc-patches not found. Expected at one of:\n  " + candidates
        + "\n\nClone it: git clone https://github.com/Cassandras-Edge/cassandra-cc-patches ~/cassandra-stack/cassandra-cc-patches"
    )
