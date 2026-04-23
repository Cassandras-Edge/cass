"""PyInstaller entry point for cass CLI."""
import multiprocessing
import warnings

# schwab-py → authlib emits AuthlibDeprecationWarning every run ("authlib.jose
# deprecated, use joserfc"). Silence it so `cass` stays quiet. authlib itself
# calls warnings.simplefilter("always", AuthlibDeprecationWarning) on import,
# which wipes any filter set before it — so import authlib.deprecate FIRST to
# let that run, then register our ignore filter on top.
import authlib.deprecate as _authlib_dep
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
