#!/usr/bin/env bash
# Install an oos release from GitHub into ~/.local/bin (Linux and macOS).
#
#   install.sh                     latest release
#   install.sh --version v0.5.0    a specific release
#   install.sh --dest /usr/local/bin
#
# Downloads with the GitHub CLI when it is logged in, otherwise with curl.
# The archive is checked against the release's SHA-256 checksums before
# anything is installed. With gh, the archive's signed build provenance is
# verified too (releases from v0.5.0 on).
set -euo pipefail

tmp=""

# Everything runs inside main, called on the last line, so a partially
# downloaded script (for example from "curl | bash") never runs part-way.
main() {
  local repo="gordonmurray/outofstate.dev" tag="" dest="${HOME}/.local/bin"
  local os arch latest base archive_name use_gh=0
  # First release with build provenance attestations.
  local first_attested="v0.5.0"

  usage() {
    cat <<'EOF'
Install an oos release from GitHub into ~/.local/bin (Linux and macOS).

  install.sh                     latest release
  install.sh --version v0.5.0    a specific release
  install.sh --dest DIR          install somewhere else
EOF
  }

  while [ $# -gt 0 ]; do
    case "$1" in
      --version) tag="${2:?--version needs a value, e.g. v0.5.0}"; shift 2 ;;
      --dest)    dest="${2:?--dest needs a directory}"; shift 2 ;;
      -h|--help) usage; exit 0 ;;
      *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
    esac
  done

  case "$(uname -s)" in
    Linux)  os=linux ;;
    Darwin) os=darwin ;;
    *) echo "oos releases are built for Linux and macOS only" >&2; exit 1 ;;
  esac

  case "$(uname -m)" in
    x86_64|amd64)  arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
  esac

  # Global, not local: the EXIT trap runs after main has returned.
  tmp="$(mktemp -d)"
  trap 'rm -rf -- "$tmp"' EXIT

  if command -v gh >/dev/null 2>&1 && gh auth status >/dev/null 2>&1; then
    use_gh=1
  fi

  if [ -z "$tag" ]; then
    if [ "$use_gh" -eq 1 ]; then
      tag="$(gh release view --repo "$repo" --json tagName --jq .tagName)"
    elif command -v curl >/dev/null 2>&1; then
      # /releases/latest redirects to /releases/tag/<tag>.
      latest="$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/${repo}/releases/latest")" || {
        echo "could not find the latest release" >&2
        exit 1
      }
      tag="${latest##*/}"
    else
      echo "install needs either gh (logged in) or curl" >&2
      exit 1
    fi
  fi
  case "$tag" in
    v[0-9]*) ;;
    *) echo "could not determine a release tag (got '${tag}')" >&2; exit 1 ;;
  esac
  archive_name="oos_${tag#v}_${os}_${arch}.tar.gz"

  if [ "$use_gh" -eq 1 ]; then
    gh release download "$tag" --repo "$repo" --pattern "$archive_name" --pattern checksums.txt --dir "$tmp"
  elif command -v curl >/dev/null 2>&1; then
    base="https://github.com/${repo}/releases/download/${tag}"
    if ! curl -fsSL -o "$tmp/$archive_name" "$base/$archive_name" ||
       ! curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt"; then
      echo "could not download ${archive_name} from release ${tag}. Check the version exists." >&2
      exit 1
    fi
  else
    echo "install needs either gh (logged in) or curl" >&2
    exit 1
  fi

  verify_checksum "$tmp" "$archive_name"

  if is_attested "$tag" "$first_attested"; then
    verify_provenance "$tmp/$archive_name" "$repo" "$tag" "$use_gh"
  fi

  tar -xzf "$tmp/$archive_name" -C "$tmp" oos

  mkdir -p "$dest"
  install -m 0755 "$tmp/oos" "$dest/oos"
  "$dest/oos" --version

  case ":${PATH}:" in
    *":${dest}:"*) ;;
    *) echo "note: ${dest} is not on your PATH. Add it with: export PATH=\"${dest}:\$PATH\"" >&2 ;;
  esac
}

# verify_checksum checks one archive against its own line in checksums.txt,
# with sha256sum (Linux) or shasum (macOS). It stops the install on mismatch.
verify_checksum() {
  local dir=$1 name=$2 line
  line="$(grep -E "^[0-9a-f]{64}  ${name//./\\.}\$" "$dir/checksums.txt")" || {
    echo "no checksum for ${name} in checksums.txt; not installing" >&2
    exit 1
  }
  if command -v sha256sum >/dev/null 2>&1; then
    (cd "$dir" && printf '%s\n' "$line" | sha256sum --check --strict -)
  elif command -v shasum >/dev/null 2>&1; then
    (cd "$dir" && printf '%s\n' "$line" | shasum -a 256 --check -)
  else
    echo "need sha256sum or shasum to verify the download" >&2
    exit 1
  fi
}

# verify_provenance checks the archive's signed build provenance with gh. It
# requires the archive to come from this repository's release workflow, built
# from the requested tag, where the installed gh supports those checks. A gh
# without attestation support only gets a note (the checksum was verified);
# a failed verification stops the install.
verify_provenance() {
  local file=$1 repo=$2 tag=$3 use_gh=$4 help
  local args=(attestation verify "$file" --repo "$repo")
  if [ "$use_gh" -ne 1 ]; then
    echo "To verify build provenance as well, install the GitHub CLI and run:" >&2
    echo "  gh attestation verify $(basename "$file") --repo ${repo}" >&2
    return 0
  fi
  if ! help="$(gh attestation verify --help 2>&1)"; then
    echo "note: this gh version cannot verify build provenance (gh 2.49 or later can); the checksum was verified" >&2
    return 0
  fi
  case "$help" in *--signer-workflow*) args+=(--signer-workflow "${repo}/.github/workflows/release.yml") ;; esac
  case "$help" in *--source-ref*) args+=(--source-ref "refs/tags/${tag}") ;; esac
  if ! gh "${args[@]}" >/dev/null; then
    echo "build provenance verification FAILED for $(basename "$file"); not installing" >&2
    exit 1
  fi
  echo "$(basename "$file"): build provenance verified"
}

# is_attested reports whether tag is at or after the first attested release.
# Version order, not SemVer: a pre-release such as v0.5.0-rc.1 also counts as
# attested, which is intended (every release from the workflow is attested).
is_attested() {
  [ "$(printf '%s\n%s\n' "$2" "$1" | sort -V | head -n1)" = "$2" ]
}

main "$@"
