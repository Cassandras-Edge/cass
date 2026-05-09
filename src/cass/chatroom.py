"""Manage the local cass-chatroom daemon — multi-agent chatroom for CC + Codex."""

from __future__ import annotations

import json
import os
import shutil
import signal
import subprocess
import sys
import time
from pathlib import Path

import click


def _state_dir() -> Path:
    override = os.environ.get("CASS_CHATROOM_STATE_DIR")
    if override:
        return Path(override).expanduser()
    if sys.platform == "darwin":
        return Path.home() / "Library" / "Application Support" / "cass-chatroom"
    xdg = os.environ.get("XDG_STATE_HOME")
    if xdg:
        return Path(xdg) / "cass-chatroom"
    return Path.home() / ".local" / "state" / "cass-chatroom"


def _socket_path() -> Path:
    return _state_dir() / "daemon.sock"


def _pid_path() -> Path:
    return _state_dir() / "daemon.pid"


def _log_path() -> Path:
    return _state_dir() / "daemon.log"


def _read_pid() -> int | None:
    p = _pid_path()
    if not p.exists():
        return None
    try:
        return int(p.read_text().strip())
    except (ValueError, OSError):
        return None


def _pid_alive(pid: int) -> bool:
    try:
        os.kill(pid, 0)
        return True
    except (ProcessLookupError, PermissionError):
        return False


def _resolve_daemon_cmd() -> list[str]:
    """Find how to invoke the daemon. Order: env override → PATH → python -m fallback."""
    override = os.environ.get("CASS_CHATROOM_DAEMON_BIN")
    if override:
        return [override]
    direct = shutil.which("cass-chatroom-daemon")
    if direct:
        return [direct]
    # Fallback: python -m cass_chatroom.daemon (requires the package importable)
    py = os.environ.get("CASS_CHATROOM_PYTHON", sys.executable or "python3")
    return [py, "-m", "cass_chatroom.daemon"]


@click.group()
def chatroom() -> None:
    """Local multi-agent chatroom — daemon, plugin, and CC entry point."""


@chatroom.command()
@click.option("--foreground", "-f", is_flag=True, help="Run in foreground instead of detaching.")
def start(foreground: bool) -> None:
    """Start the chatroom daemon."""
    pid = _read_pid()
    if pid is not None and _pid_alive(pid):
        click.echo(f"already running (pid={pid})")
        return

    cmd = _resolve_daemon_cmd()

    if foreground:
        click.echo(f"running daemon in foreground: {' '.join(cmd)}")
        os.execvp(cmd[0], cmd)
        return

    state = _state_dir()
    state.mkdir(parents=True, exist_ok=True)
    log = _log_path().open("a")
    proc = subprocess.Popen(
        cmd,
        stdout=log,
        stderr=subprocess.STDOUT,
        stdin=subprocess.DEVNULL,
        start_new_session=True,
    )

    # Wait briefly for the socket to appear
    deadline = time.time() + 5.0
    sock = _socket_path()
    while time.time() < deadline:
        if sock.exists():
            click.echo(f"daemon started (pid={proc.pid}, socket={sock})")
            return
        if proc.poll() is not None:
            raise click.ClickException(
                f"daemon exited immediately (rc={proc.returncode}). Check {_log_path()}"
            )
        time.sleep(0.1)
    raise click.ClickException(f"daemon did not create socket within 5s. Check {_log_path()}")


@chatroom.command()
@click.option("--force", is_flag=True, help="SIGKILL instead of SIGTERM.")
def stop(force: bool) -> None:
    """Stop the chatroom daemon."""
    pid = _read_pid()
    if pid is None:
        click.echo("no daemon running (no pid file)")
        return
    if not _pid_alive(pid):
        click.echo(f"pid {pid} is not alive; cleaning up state files")
        _pid_path().unlink(missing_ok=True)
        _socket_path().unlink(missing_ok=True)
        return

    sig = signal.SIGKILL if force else signal.SIGTERM
    click.echo(f"sending {sig.name} to pid {pid}")
    os.kill(pid, sig)

    # Wait for it to exit
    deadline = time.time() + 10.0
    while time.time() < deadline:
        if not _pid_alive(pid):
            _pid_path().unlink(missing_ok=True)
            _socket_path().unlink(missing_ok=True)
            click.echo("stopped")
            return
        time.sleep(0.1)
    raise click.ClickException(f"daemon did not exit within 10s; try --force")


@chatroom.command()
def status() -> None:
    """Show daemon status, rooms, and agents."""
    pid = _read_pid()
    sock = _socket_path()
    if pid is None:
        click.echo("daemon: not running (no pid file)")
        return
    alive = _pid_alive(pid)
    socket_ok = sock.exists()

    click.echo(f"daemon: pid={pid} alive={alive} socket={'present' if socket_ok else 'missing'}")
    click.echo(f"  state dir: {_state_dir()}")
    click.echo(f"  log: {_log_path()}")

    if alive and socket_ok:
        rooms_dir = _state_dir() / "rooms"
        if rooms_dir.exists():
            rooms = sorted(p.name for p in rooms_dir.iterdir() if p.is_dir())
            if rooms:
                click.echo("\nrooms (from disk):")
                for r in rooms:
                    log = rooms_dir / r / "log.jsonl"
                    msg_count = sum(1 for _ in log.open()) if log.exists() else 0
                    click.echo(f"  {r} ({msg_count} messages)")
            else:
                click.echo("\nno rooms yet")


@chatroom.command()
@click.argument("claude_args", nargs=-1, type=click.UNPROCESSED)
@click.option("--room-id", help="Override room id (default: derived from CC session).")
@click.option("--role", type=click.Choice(["admin", "worker"]), default="admin", help="Connect as admin or worker.")
@click.option("--dev/--no-dev", default=True, help="Use --dangerously-load-development-channels (required until plugin is on Anthropic allowlist).")
def claude(claude_args: tuple[str, ...], room_id: str | None, role: str, dev: bool) -> None:
    """Start Claude Code with the chatroom plugin enabled.

    Auto-starts the daemon if it isn't running. All extra args are forwarded to claude.

    Example:
      cass chatroom claude
      cass chatroom claude --role worker --room-id ccs-018f-...
    """
    # Ensure daemon is up
    pid = _read_pid()
    if pid is None or not _pid_alive(pid):
        click.echo("daemon not running; starting...")
        ctx = click.get_current_context()
        ctx.invoke(start, foreground=False)

    claude_bin = shutil.which("claude")
    if not claude_bin:
        raise click.ClickException("claude CLI not found in PATH")

    plugin_ref = "plugin:cass-chatroom@cass-chatroom"
    flag = "--dangerously-load-development-channels" if dev else "--channels"

    env = os.environ.copy()
    env["CASS_CHATROOM_ROLE"] = role
    if room_id:
        env["CASS_CHATROOM_ROOM_ID"] = room_id

    cmd = [claude_bin, flag, plugin_ref, *claude_args]
    click.echo(f"exec: {' '.join(cmd)}")
    os.execvpe(claude_bin, cmd, env)


@chatroom.command()
@click.option("-n", "--lines", default=50, help="Number of lines to tail.")
@click.option("-f", "--follow", is_flag=True, help="Follow the log (like tail -f).")
def logs(lines: int, follow: bool) -> None:
    """Tail the daemon log."""
    log = _log_path()
    if not log.exists():
        raise click.ClickException(f"no log file at {log}")
    args = ["tail", f"-n{lines}"]
    if follow:
        args.append("-f")
    args.append(str(log))
    os.execvp("tail", args)
