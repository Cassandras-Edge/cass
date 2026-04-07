"""Cassandra platform CLI."""

from __future__ import annotations

import click

from cass.auth import login, logout, whoami
from cass.cookies import cookies
from cass.keys import keys


@click.group()
@click.version_option()
def main() -> None:
    """Cassandra platform CLI — cookie sync, MCP key management."""


main.add_command(login)
main.add_command(logout)
main.add_command(whoami)
main.add_command(cookies)
main.add_command(keys)
