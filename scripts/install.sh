#!/usr/bin/env bash
# Install an oos release from GitHub into ~/.local/bin.
#
#   install.sh                     latest release
#   install.sh --version v0.3.1    a specific release
#   install.sh --dest /usr/local/bin
#
# Downloads with the GitHub CLI when it is logged in (works for private
# repositories), otherwise with curl (public repositories only).
# Needs: sha256sum, tar, and gh or curl.
set -euo pipefail

repo="gordonmurray/outofstate.dev"
tag=""
dest="${HOME}/.local/bin"

usage() {
  cat <<'EOF'
Install an oos release from GitHub into ~/.local/bin.

  install.sh                     latest release
  install.sh --version v0.3.1    a specific release
  install.sh --dest DIR          install somewhere else
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --version) tag="${2:?--version needs a value, e.g. v0.3.1}"; shift 2 ;;
    --dest)    dest="${2:?--dest needs a directory}"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

if [ "$(uname -s)" != "Linux" ]; then
  echo "oos releases are currently built for Linux only" >&2
  exit 1
fi

case "$(uname -m)" in
  x86_64|amd64)  arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

tmp="$(mktemp -d)"
trap 'rm -rf -- "$tmp"' EXIT

if command -v gh >/dev/null 2>&1 && gh auth status >/dev/null 2>&1; then
  # An empty tag means the latest release.
  gh release download ${tag:+"$tag"} --repo "$repo" \
    --pattern "oos_*_linux_${arch}.tar.gz" --pattern checksums.txt --dir "$tmp"
elif command -v curl >/dev/null 2>&1; then
  if [ -z "$tag" ]; then
    # /releases/latest redirects to /releases/tag/<tag>.
    latest="$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/${repo}/releases/latest")" || {
      echo "could not find the latest release. If the repository is private, install gh and run 'gh auth login'." >&2
      exit 1
    }
    tag="${latest##*/}"
  fi
  case "$tag" in
    v[0-9]*) ;;
    *) echo "could not determine a release tag (got '${tag}'). If the repository is private, install gh and run 'gh auth login'." >&2; exit 1 ;;
  esac
  base="https://github.com/${repo}/releases/download/${tag}"
  archive_name="oos_${tag#v}_linux_${arch}.tar.gz"
  if ! curl -fsSL -o "$tmp/$archive_name" "$base/$archive_name" ||
     ! curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt"; then
    echo "could not download release ${tag}. Check the version exists. If the repository is private, install gh and run 'gh auth login'." >&2
    exit 1
  fi
else
  echo "install needs either gh (logged in) or curl" >&2
  exit 1
fi

# Stops here (set -e) if the archive does not match its checksum.
(cd "$tmp" && sha256sum --check --ignore-missing --strict checksums.txt)

archive=("$tmp"/oos_*_linux_"${arch}".tar.gz)
tar -xzf "${archive[0]}" -C "$tmp" oos

mkdir -p "$dest"
install -m 0755 "$tmp/oos" "$dest/oos"
"$dest/oos" --version

case ":${PATH}:" in
  *":${dest}:"*) ;;
  *) echo "note: ${dest} is not on your PATH. Add it with: export PATH=\"${dest}:\$PATH\"" >&2 ;;
esac
