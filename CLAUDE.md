# cass

Go CLI for the Cassandra platform (Cobra + Charm). Entry point: `cmd/cass`.

## Build

- `scripts/build.sh` → builds `bin/cass` with the version stamped from `git describe`.
  Optionally bakes Gmail OAuth client via `CASS_GMAIL_CLIENT_ID` / `CASS_GMAIL_CLIENT_SECRET`.
- Quick check: `go build ./...` and `go test ./...`.
- Never commit binaries — `cass`, `bin/`, `dist/`, `build/` are gitignored.

## Release

- `scripts/release.sh vX.Y.Z` — cross-compiles all platforms, tags, and creates a GitHub
  release (`--dry` to build without tagging). Requires a clean tree and authed `gh`.
- `.github/workflows/release.yml` runs the GitHub-side release workflow on tag push.

## Layout

- `internal/cmd/` — one file per command (Cobra). `root.go` wires everything;
  `Version` is injected via ldflags.
- `internal/registry/` — the service catalog (known Cassandra services/MCPs the CLI talks to).
- Other support packages: `internal/auth`, `internal/portal`, `internal/config`,
  `internal/browser`, `internal/manifest`.
