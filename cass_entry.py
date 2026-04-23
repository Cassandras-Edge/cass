"""PyInstaller entry point for cass CLI."""
import os
import sys


def _apply_pending_update() -> None:
    """If a prior `cass update` staged `<cass>.pending`, swap it in now and
    re-exec into the new binary before any lazy imports happen.

    Doing this here is the safe window for a one-file PyInstaller bundle:
    we have only touched `os` and `sys`, and the immediate `os.execv` tears
    down the current process. That sidesteps the zlib-decompress crash that
    would otherwise happen if we replaced the file mid-run — PyInstaller
    reads its embedded archive from the binary file on demand, and a swap
    mid-execution invalidates its cached archive offsets.
    """
    if not getattr(sys, "frozen", False):
        return  # dev / source run — nothing to do
    try:
        exe = os.path.realpath(sys.executable)
        pending = exe + ".pending"
        if not os.path.exists(pending):
            return
        os.replace(pending, exe)
        os.chmod(exe, 0o755)
    except OSError:
        # Best-effort cleanup — if the swap fails, drop the pending file so
        # we don't loop forever on a broken download.
        try:
            os.unlink(exe + ".pending")
        except OSError:
            pass
        return
    os.execv(exe, [exe] + sys.argv[1:])


_apply_pending_update()


import multiprocessing  # noqa: E402
import warnings  # noqa: E402

# schwab-py → authlib emits AuthlibDeprecationWarning every run ("authlib.jose
# deprecated, use joserfc"). Silence it so `cass` stays quiet. authlib itself
# calls warnings.simplefilter("always", AuthlibDeprecationWarning) on import,
# which wipes any filter set before it — so import authlib.deprecate FIRST to
# let that run, then register our ignore filter on top.
import authlib.deprecate as _authlib_dep  # noqa: E402
warnings.filterwarnings("ignore", category=_authlib_dep.AuthlibDeprecationWarning)
del _authlib_dep

if __name__ == "__main__":
    # On Windows frozen builds, multiprocessing.spawn re-execs this binary
    # with `--multiprocessing-fork <fd>` extra args. freeze_support() detects
    # and handles that handoff; without it, Click parses the flag as CLI input
    # and the child never starts (breaks schwab-py's OAuth callback server).
    multiprocessing.freeze_support()
    from cass.cli import main
    main()
