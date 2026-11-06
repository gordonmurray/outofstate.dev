#!/usr/bin/env bash
# Install an oos release from GitHub into ~/.local/bin.
#
#   scripts/install.sh                   latest release
#   scripts/install.sh --version v0.3.1  a specific release
#
# Needs: gh (authenticated with "gh auth login"), sha256sum, tar.
set -euo pipefail

repo="gordonmurray/outofstate.dev"
tag=""
dest="${HOME}/.local/bin"

while [ $# -gt 0 ]; do
  case "$1" in
    --version) tag="${2:?--version needs a value, e.g. v0.3.1}"; shift 2 ;;
    --dest)    dest="${2:?--dest needs a directory}"; shift 2 ;;
    -h|--help) sed -n '2,7p' "$0"; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

case "$(uname -m)" in
  x86_64|amd64)  arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

tmp="$(mktemp -d)"
trap 'rm -rf -- "$tmp"' EXIT

# An empty tag means the latest release.
gh release download ${tag:+"$tag"} --repo "$repo" \
  --pattern "oos_*_linux_${arch}.tar.gz" --pattern checksums.txt --dir "$tmp"

# Stops here (set -e) if the archive does not match its checksum.
(cd "$tmp" && sha256sum --check --ignore-missing --strict checksums.txt)

archive=("$tmp"/oos_*_linux_"${arch}".tar.gz)
tar -xzf "${archive[0]}" -C "$tmp" oos

mkdir -p "$dest"
install -m 0755 "$tmp/oos" "$dest/oos"
"$dest/oos" --version

case ":${PATH}:" in
  *":${dest}:"*) ;;
  *) echo "note: ${dest} is not on your PATH" >&2 ;;
esac
