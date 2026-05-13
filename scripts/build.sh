#!/usr/bin/env bash
# Build cass for the current host, with version stamped from git.
#
# Usage:
#   scripts/build.sh           # writes bin/cass
#   scripts/build.sh /path     # writes to /path

set -euo pipefail
cd "$(dirname "$0")/.."

VERSION="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
OUT="${1:-bin/cass}"
mkdir -p "$(dirname "$OUT")"

# Optional: bake the Gmail OAuth client (Desktop type — secret isn't truly
# confidential per Google's docs) into the binary so `cass gmail link` works
# out of the box. Can also be supplied at runtime via
# CASS_GMAIL_CLIENT_ID/SECRET env vars.
LDFLAGS="-s -w -X github.com/Cassandras-Edge/cass/internal/cmd.Version=${VERSION}"
if [[ -n "${CASS_GMAIL_CLIENT_ID:-}" && -n "${CASS_GMAIL_CLIENT_SECRET:-}" ]]; then
  LDFLAGS="${LDFLAGS} -X github.com/Cassandras-Edge/cass/internal/cmd.gmailClientID=${CASS_GMAIL_CLIENT_ID}"
  LDFLAGS="${LDFLAGS} -X github.com/Cassandras-Edge/cass/internal/cmd.gmailClientSecret=${CASS_GMAIL_CLIENT_SECRET}"
fi

go build \
  -ldflags="${LDFLAGS}" \
  -o "$OUT" ./cmd/cass

echo "built: $OUT ($VERSION, $(du -h "$OUT" | cut -f1))"
