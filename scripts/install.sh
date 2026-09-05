#!/usr/bin/env bash
# Installs the chronos-code binary from a GitHub Release.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/spawn08/chronos-code/main/scripts/install.sh | bash
#
# Env overrides:
#   VERSION     release tag to install, e.g. v1.2.3 (default: latest)
#   INSTALL_DIR directory to install the binary into (default: ~/.local/bin)

set -euo pipefail

REPO="spawn08/chronos-code"
VERSION="${VERSION:-latest}"
INSTALL_DIR="${INSTALL_DIR:-$HOME/.local/bin}"

os="$(uname -s)"
case "$os" in
  Linux)  goos="linux" ;;
  Darwin) goos="darwin" ;;
  *)
    echo "error: unsupported OS: $os (use scripts/install.ps1 on Windows)" >&2
    exit 1
    ;;
esac

arch="$(uname -m)"
case "$arch" in
  x86_64|amd64)  goarch="amd64" ;;
  arm64|aarch64) goarch="arm64" ;;
  *)
    echo "error: unsupported architecture: $arch" >&2
    exit 1
    ;;
esac

if [ "$VERSION" = "latest" ]; then
  tag="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" | grep -m1 '"tag_name"' | cut -d'"' -f4)"
  if [ -z "$tag" ]; then
    echo "error: could not resolve latest release tag" >&2
    exit 1
  fi
else
  tag="$VERSION"
fi

archive="chronos-code-${tag}-${goos}-${goarch}.tar.gz"
base_url="https://github.com/${REPO}/releases/download/${tag}"

workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT

echo "Downloading ${archive} (${tag})..."
curl -fsSL "${base_url}/${archive}" -o "${workdir}/${archive}"
curl -fsSL "${base_url}/checksums-sha256.txt" -o "${workdir}/checksums-sha256.txt"

echo "Verifying checksum..."
( cd "$workdir" && grep " ${archive}\$" checksums-sha256.txt | sha256sum -c - )

echo "Installing to ${INSTALL_DIR}..."
mkdir -p "$INSTALL_DIR"
tar xzf "${workdir}/${archive}" -C "$workdir" chronos-code
install -m 755 "${workdir}/chronos-code" "${INSTALL_DIR}/chronos-code"

echo "Installed chronos-code ${tag} to ${INSTALL_DIR}/chronos-code"
case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *) echo "Note: ${INSTALL_DIR} is not on your PATH. Add it, e.g.: export PATH=\"${INSTALL_DIR}:\$PATH\"" ;;
esac
