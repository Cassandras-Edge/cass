"""Cassandra platform CLI."""

from __future__ import annotations

import json
import os
import time
from pathlib import Path

import click

from cass.auth import login, logout, whoami
from cass.cookies import cookies
from cass.discord import discord
from cass.ensure import ensure_key
from cass.keys import keys
from cass.setup import setup
from cass.update import update, auto_update_check, CURRENT_VERSION


# Check for updates at most once per hour
UPDATE_CHECK_INTERVAL = 3600
UPDATE_STATE_FILE = Path.home() / ".config" / "cass" / ".update-check"


def _should_check_update() -> bool:
    """Rate-limit update checks to once per hour."""
    if os.environ.get("CASS_NO_AUTO_UPDATE"):
        return False
    try:
        if UPDATE_STATE_FILE.exists():
            last_check = float(UPDATE_STATE_FILE.read_text().strip())
            if time.time() - last_check < UPDATE_CHECK_INTERVAL:
                return False
    except (ValueError, OSError):
        pass
    return True


def _mark_update_checked() -> None:
    try:
        UPDATE_STATE_FILE.parent.mkdir(parents=True, exist_ok=True)
        UPDATE_STATE_FILE.write_text(str(time.time()))
    except OSError:
        pass


@click.group()
@click.version_option(version=CURRENT_VERSION, prog_name="cass")
def main() -> None:
    """Cassandra platform CLI — auth, keys, cookies, and service management."""
    if _should_check_update():
        _mark_update_checked()
        auto_update_check()


main.add_command(login)
main.add_command(logout)
main.add_command(whoami)
main.add_command(cookies)
main.add_command(discord)
main.add_command(ensure_key)
main.add_command(keys)
main.add_command(setup)
main.add_command(update)
