#!/usr/bin/env bash
# Smoke test for a built oos binary on the current platform (used in CI on
# macOS runners). Needs only bash, jq and the repository checkout.
#   test/smoke.sh path/to/oos expected-goos/expected-goarch
set -euo pipefail

oos="$1"
want_platform="$2"
repo="$(cd "$(dirname "$0")/.." && pwd)"
td="$repo/cmd/oos/testdata"
fake="$repo/internal/tfrun/testdata/fake-terraform"
fails=0

check() {
  local name=$1 want=$2 got=$3
  if [ "$got" = "$want" ]; then echo "ok   $name"; else echo "FAIL $name: got '$got', want '$want'"; fails=$((fails + 1)); fi
}

version="$("$oos" --version)"
echo "$version"
case "$version" in *"$want_platform"*) check "platform" yes yes ;; *) check "platform" "$want_platform" "$version" ;; esac

code=0
"$oos" --inventory-file "$td/inventory.json" --state-file "$td/app.tfstate.json" --state-file "$td/shared.tfstate.json" \
  --format json --output "$RUNNER_TEMP/report.json" || code=$?
check "fixture scan exit code" 2 "$code"
check "import candidate" analytics-db "$(jq -r '[.findings[]|select(.status=="no_match")|.identifier]|join(",")' "$RUNNER_TEMP/report.json")"

dir="$(mktemp -d)"
code=0
FAKE_MODE=ok FAKE_STATE="$td/app.tfstate.json" TF_WORKSPACE=prod \
  "$oos" --inventory-file "$td/inventory.json" --tf-binary "$fake" --source "$dir" --format json --output "$RUNNER_TEMP/source.json" || code=$?
check "--source exit code" 2 "$code"
check "--source workspace" prod "$(jq -r '.sources[0].workspace' "$RUNNER_TEMP/source.json")"

start=$(date +%s)
code=0
FAKE_MODE=hang "$oos" --inventory-file "$td/inventory.json" --tf-binary "$fake" --source "$dir" \
  --source-timeout 1s --format json --output "$RUNNER_TEMP/hang.json" || code=$?
elapsed=$(( $(date +%s) - start ))
check "hung terraform exit code" 3 "$code"
check "hung terraform category" timeout "$(jq -r '.sources[0].error_category' "$RUNNER_TEMP/hang.json")"
check "hung terraform killed promptly" yes "$( [ "$elapsed" -le 10 ] && echo yes || echo "no (${elapsed}s)")"

if [ "$fails" -ne 0 ]; then echo "$fails failure(s)"; exit 1; fi
echo "smoke test passed"
