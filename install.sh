#!/usr/bin/env bash
# Install cass — Cassandra platform CLI
#
# Public repo:  curl -sSL https://raw.githubusercontent.com/Cassandras-Edge/cass/main/install.sh | sh
# Private repo: gh api repos/Cassandras-Edge/cass/contents/install.sh --jq '.content' | base64 -d | sh
# Or just:      gh release download --repo Cassandras-Edge/cass --pattern 'cass-*' --dir ~/.local/bin

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

# Try gh CLI first (works with private repos), fall back to curl
if command -v gh &>/dev/null && gh auth status &>/dev/null 2>&1; then
  echo "Fetching latest release via gh..."
  mkdir -p "$INSTALL_DIR"
  gh release download --repo "$REPO" --pattern "$ASSET" --dir "$INSTALL_DIR" --clobber 2>/dev/null || {
    # Might be named with .exe on Windows
    gh release download --repo "$REPO" --pattern "${ASSET}.exe" --dir "$INSTALL_DIR" --clobber
  }
  mv "${INSTALL_DIR}/${ASSET}" "${INSTALL_DIR}/cass" 2>/dev/null || true
  chmod +x "${INSTALL_DIR}/cass"
  VERSION=$(gh release view --repo "$REPO" --json tagName --jq '.tagName')
else
  echo "Fetching latest release..."
  RELEASE=$(curl -sL "https://api.github.com/repos/${REPO}/releases/latest")
  VERSION=$(echo "$RELEASE" | grep -o '"tag_name": *"[^"]*"' | head -1 | cut -d'"' -f4)
  URL=$(echo "$RELEASE" | grep -o '"browser_download_url": *"[^"]*'"${ASSET}"'[^"]*"' | head -1 | cut -d'"' -f4)

  if [ -z "$URL" ]; then
    echo "No binary found for ${TARGET}" >&2
    echo "If this is a private repo, install gh CLI and run: gh auth login" >&2
    echo "Then re-run this script." >&2
    exit 1
  fi

  echo "Downloading cass ${VERSION} for ${TARGET}..."
  mkdir -p "$INSTALL_DIR"
  curl -sL "$URL" -o "${INSTALL_DIR}/cass"
  chmod +x "${INSTALL_DIR}/cass"
fi

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
