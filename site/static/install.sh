#!/usr/bin/env bash
set -euo pipefail

REPO="google/sam"
INSTALL_DIR="/usr/local/bin"

echo "Installing SAM from $REPO..."

# Get OS and Arch
OS="$(uname -s)"
case "${OS}" in
    Linux*)     OS_NAME="Linux";;
    Darwin*)    OS_NAME="Darwin";;
    *)          echo "Unsupported OS: ${OS}"; exit 1;;
esac

ARCH="$(uname -m)"
case "${ARCH}" in
    x86_64*)    ARCH_NAME="x86_64";;
    aarch64*)   ARCH_NAME="arm64";;
    arm64*)     ARCH_NAME="arm64";;
    *)          echo "Unsupported architecture: ${ARCH}"; exit 1;;
esac

# Get latest release version
echo "Fetching latest release information..."
LATEST_RELEASE_URL="https://api.github.com/repos/${REPO}/releases/latest"
# `|| true`: with pipefail, a grep that matches nothing would kill the script
# here instead of reaching the friendly error below.
VERSION=$(curl -s $LATEST_RELEASE_URL | grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/' || true)

if [ -z "$VERSION" ]; then
    echo "Error: Could not find the latest release."
    exit 1
fi

echo "Found latest version: ${VERSION}"

# Construct download URL (matches goreleaser name template)
TAR_NAME="sam_${OS_NAME}_${ARCH_NAME}.tar.gz"
DOWNLOAD_URL="https://github.com/${REPO}/releases/download/${VERSION}/${TAR_NAME}"

# Create a temporary directory
TMP_DIR=$(mktemp -d)
trap 'rm -rf "$TMP_DIR"' EXIT
cd "$TMP_DIR"

echo "Downloading ${DOWNLOAD_URL}..."
if ! curl -sfL -o "${TAR_NAME}" "${DOWNLOAD_URL}"; then
    echo "Error: Failed to download ${DOWNLOAD_URL}"
    exit 1
fi

echo "Extracting..."
tar -xzf "${TAR_NAME}"

echo "Installing to ${INSTALL_DIR} (may require sudo)..."
INSTALLED_BINS=()
for b in sam-node sam-control-plane sam-router mcp-client sam-box sam-console nano-init; do
    if [ -f "$b" ]; then
        INSTALLED_BINS+=("$b")
    fi
done

if [ ${#INSTALLED_BINS[@]} -eq 0 ]; then
    echo "Error: No SAM binaries found in release archive."
    exit 1
fi

if [ -w "$INSTALL_DIR" ]; then
    mv "${INSTALLED_BINS[@]}" "$INSTALL_DIR/"
else
    sudo mv "${INSTALLED_BINS[@]}" "$INSTALL_DIR/"
fi

# Cleanup
cd - > /dev/null
rm -rf "$TMP_DIR"

echo "Successfully installed SAM (${VERSION}) to ${INSTALL_DIR}"
echo "Run 'sam-node --help' to get started."
