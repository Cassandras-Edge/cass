#!/usr/bin/env bash
# Install cass — Cassandra platform CLI
# Usage: curl -sSL https://raw.githubusercontent.com/Cassandras-Edge/cass/main/install.sh | sh

set -euo pipefail

REPO="Cassandras-Edge/cass"
INSTALL_DIR="${CASS_INSTALL_DIR:-$HOME/.local/bin}"

# Detect platform
OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"

case "$ARCH" in
  x86_64|amd64) ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *) echo "Unsupported architecture: $ARCH" >&2; exit 1 ;;
esac

TARGET="${OS}-${ARCH}"
ASSET="cass-${TARGET}"

# Get latest release
echo "Fetching latest release..."
RELEASE=$(curl -sL "https://api.github.com/repos/${REPO}/releases/latest")
VERSION=$(echo "$RELEASE" | grep -o '"tag_name": *"[^"]*"' | head -1 | cut -d'"' -f4)
URL=$(echo "$RELEASE" | grep -o '"browser_download_url": *"[^"]*'"${ASSET}"'[^"]*"' | head -1 | cut -d'"' -f4)

if [ -z "$URL" ]; then
  echo "No binary found for ${TARGET}" >&2
  echo "Available at: https://github.com/${REPO}/releases/latest" >&2
  exit 1
fi

# Download
echo "Downloading cass ${VERSION} for ${TARGET}..."
mkdir -p "$INSTALL_DIR"
curl -sL "$URL" -o "${INSTALL_DIR}/cass"
chmod +x "${INSTALL_DIR}/cass"

echo ""
echo "Installed cass ${VERSION} to ${INSTALL_DIR}/cass"

# Check PATH
if ! echo "$PATH" | tr ':' '\n' | grep -qx "$INSTALL_DIR"; then
  echo ""
  echo "Add to your PATH:"
  echo "  export PATH=\"${INSTALL_DIR}:\$PATH\""
  echo ""
  echo "Add that line to ~/.bashrc or ~/.zshrc to make it permanent."
fi

echo ""
echo "Get started:"
echo "  cass login        # authenticate with the platform"
echo "  cass --help       # see all commands"
