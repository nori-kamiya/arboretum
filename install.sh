#!/bin/sh
# orchard installer — fetches the latest release binary for your Mac.
#
#   curl -fsSL https://raw.githubusercontent.com/nori-kamiya/orchard/main/install.sh | sh
#
# Override the install dir with BINDIR=... (default: $HOME/.local/bin).
set -eu

REPO="nori-kamiya/orchard"
BINDIR="${BINDIR:-$HOME/.local/bin}"

os="$(uname -s)"
arch="$(uname -m)"

[ "$os" = "Darwin" ] || { echo "orchard requires macOS (got $os)" >&2; exit 1; }
case "$arch" in
  arm64)  goarch=arm64 ;;
  x86_64) goarch=amd64 ;;
  *) echo "unsupported architecture: $arch" >&2; exit 1 ;;
esac

echo "resolving latest orchard release..."
tag="$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
  | grep '"tag_name"' | head -1 | cut -d'"' -f4)"
[ -n "$tag" ] || { echo "could not determine the latest release" >&2; exit 1; }
ver="${tag#v}"

base="https://github.com/$REPO/releases/download/$tag"
asset="orchard_${ver}_darwin_${goarch}.tar.gz"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

echo "downloading $asset ..."
curl -fsSL "$base/$asset" -o "$tmp/$asset"

# Verify the checksum when the checksums file is available.
if curl -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt" 2>/dev/null; then
  ( cd "$tmp" && grep " $asset\$" checksums.txt | shasum -a 256 -c - ) \
    || { echo "checksum verification failed" >&2; exit 1; }
fi

tar -xzf "$tmp/$asset" -C "$tmp"
mkdir -p "$BINDIR"
install -m 0755 "$tmp/orchard" "$BINDIR/orchard"

echo "installed orchard $tag to $BINDIR/orchard"
case ":$PATH:" in
  *":$BINDIR:"*) ;;
  *) echo "note: $BINDIR is not on your PATH — add it to your shell profile." ;;
esac
echo "run 'orchard version' to verify, and 'orchard up --dry-run' to try it out."
