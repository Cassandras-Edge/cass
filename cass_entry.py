"""PyInstaller entry point for cass CLI."""
import multiprocessing

if __name__ == "__main__":
    # On Windows frozen builds, multiprocessing.spawn re-execs this binary
    # with `--multiprocessing-fork <fd>` extra args. freeze_support() detects
    # and handles that handoff; without it, Click parses the flag as CLI input
    # and the child never starts (breaks schwab-py's OAuth callback server).
    multiprocessing.freeze_support()
    from cass.cli import main
    main()
