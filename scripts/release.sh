#!/usr/bin/env bash
# Build cass for every supported platform and create a GitHub release.
#
# Usage:
#   scripts/release.sh v0.8.0        # tag + build + upload to GH release
#   scripts/release.sh v0.8.0 --dry  # build artifacts but don't tag/push
#
# Requires:
#   - gh CLI authenticated (`gh auth status`)
#   - clean working tree (no uncommitted changes)
#   - a version tag like v0.8.0 as $1

set -euo pipefail
cd "$(dirname "$0")/.."

if [ $# -lt 1 ]; then
  echo "usage: $0 <tag> [--dry]" >&2
  exit 1
fi

TAG="$1"
DRY="${2:-}"

if ! [[ "$TAG" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[a-z0-9]+)?$ ]]; then
  echo "tag must look like v0.8.0 or v0.8.0-rc1" >&2
  exit 1
fi

if [ "$DRY" != "--dry" ] && [ -n "$(git status --porcelain)" ]; then
  echo "working tree must be clean to cut a release" >&2
  git status --short
  exit 1
fi

DIST="dist/$TAG"
rm -rf "$DIST"
mkdir -p "$DIST"

# Build matrix — keep in sync with .github/workflows/release.yml, otherwise
# the scoop bump job loses its cass-windows-amd64.exe input on a manual
# local release.
matrix=(
  "darwin  amd64  "
  "darwin  arm64  "
  "linux   amd64  "
  "linux   arm64  "
  "windows amd64 .exe"
)

for entry in "${matrix[@]}"; do
  read -r GOOS GOARCH EXT <<< "$entry"
  OUT="$DIST/cass-${GOOS}-${GOARCH}${EXT}"
  echo "→ $OUT"
  GOOS="$GOOS" GOARCH="$GOARCH" CGO_ENABLED=0 \
    go build \
      -trimpath \
      -ldflags="-s -w -X github.com/Cassandras-Edge/cass/internal/cmd.Version=${TAG}" \
      -o "$OUT" ./cmd/cass
done

ls -lh "$DIST"

if [ "$DRY" = "--dry" ]; then
  echo
  echo "dry run — skipping tag + GH release. Artifacts at $DIST"
  exit 0
fi

# Tag + push + GH release.
git tag -a "$TAG" -m "$TAG"
git push origin "$TAG"

gh release create "$TAG" \
  --title "$TAG" \
  --notes "Cass $TAG — Go binary release" \
  "$DIST"/cass-*

echo
echo "released $TAG"
