#!/usr/bin/env bash
# check() evaluates its single-quoted first argument later, on purpose.
# shellcheck disable=SC2016
# Self-test for test/e2e/run.sh with fake aws and terraform commands. Checks
# the safety rules without touching AWS. Run with: make e2e-selftest
set -uo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
failures=0

# scenario NAME EXPECTED_EXIT [ENV=VALUE...] -- runs run.sh in a fresh sandbox.
scenario() {
  local name=$1 want=$2; shift 2
  sandbox="$(mktemp -d)"
  export FAKE_DB="$sandbox/db.json" FAKE_CALLS="$sandbox/calls" ARTIFACTS="$sandbox/artifacts"
  echo '{"DBInstances": []}' > "$FAKE_DB"
  : > "$FAKE_CALLS"
  [ -n "${SEED:-}" ] && echo "$SEED" > "$FAKE_DB"
  set +e
  env PATH="$here/selftest:$PATH" AWS_PROFILE=fake AWS_REGION=eu-west-1 E2E_CONFIRM=yes \
    E2E_DIR="$here" OOS_BIN=/bin/false "$@" bash "$here/run.sh" > "$sandbox/out" 2>&1 &
  local pid=$!
  if [ -n "${SIGNAL_AFTER:-}" ]; then sleep "$SIGNAL_AFTER"; kill -TERM "$pid"; fi
  wait "$pid"; got=$?
  set -e
  if [ "$got" -ne "$want" ]; then
    echo "FAIL $name: exit $got, want $want"; sed 's/^/    /' "$sandbox/out"; failures=$((failures+1))
  else
    echo "ok   $name"
  fi
}
check() { if ! eval "$1"; then echo "FAIL   $2"; failures=$((failures+1)); fi; }
remaining() { jq -r '[.DBInstances[].DBInstanceIdentifier] | sort | join(",")' "$FAKE_DB"; }

set -e

scenario "hostile run id is rejected before any AWS call" 2 RUN_ID='x"||true'
check '[ ! -s "$FAKE_CALLS" ]' "no command may run for a hostile RUN_ID"

scenario "no confirmation, no AWS call" 2 E2E_CONFIRM=no
check '[ ! -s "$FAKE_CALLS" ]' "no command may run without E2E_CONFIRM=yes"

SEED='{"DBInstances":[{"DBInstanceIdentifier":"someone-elses-db","TagList":[]}]}' \
  scenario "non-empty region stops before creating or deleting anything" 1 RUN_ID=r1
check '! grep -qE "delete-db-instance|terraform .* (apply|destroy)" "$FAKE_CALLS"' "preflight failure must not apply, destroy or delete"
check '[ "$(remaining)" = "someone-elses-db" ]' "the unrelated instance must remain"

scenario "apply B fails: both applies settle, everything is cleaned up, no PASS" 1 RUN_ID=r2 FAKE_APPLY_FAIL=project-b
check '[ "$(remaining)" = "" ]' "no run instances may remain"
check '! grep -q PASS "$sandbox/out"' "must not print PASS"

# Destroy and delete both fail; unrelated instances appear after preflight.
# Cleanup must exit non-zero, save state, and never touch the others.
scenario "failed cleanup exits 3, keeps state, deletes only run-owned instances" 3 RUN_ID=r3 \
  FAKE_APPLY_FAIL=project-a FAKE_DESTROY_FAIL=1 FAKE_DELETE_FAIL=1
check 'grep -q "CLEANUP FAILED" "$sandbox/out"' "must report the cleanup failure"
check '[ -d "$ARTIFACTS/r3/project-a" ]' "must save Terraform state for recovery"
check '! grep -q PASS "$sandbox/out"' "must not print PASS"

SEED_EXTRA='[{"DBInstanceIdentifier":"oos-e2e-r4-x-untagged","TagList":[]},{"DBInstanceIdentifier":"other-db","TagList":[{"Key":"oos-e2e","Value":"r4"}]}]'
scenario "tag cleanup ignores instances that only match the prefix or only the tag" 1 RUN_ID=r4 \
  FAKE_APPLY_FAIL=project-b FAKE_DESTROY_FAIL=1 FAKE_INJECT="$SEED_EXTRA"
check '! grep -q "delete-db-instance --db-instance-identifier oos-e2e-r4-x-untagged" "$FAKE_CALLS"' "must not delete an untagged instance with the prefix"
check '! grep -q "delete-db-instance --db-instance-identifier other-db" "$FAKE_CALLS"' "must not delete a tagged instance without the prefix"
check 'grep -q "delete-db-instance --db-instance-identifier oos-e2e-r4-a1" "$FAKE_CALLS"' "must delete the run-owned instance"

SIGNAL_AFTER=2 scenario "SIGTERM during apply interrupts terraform and cleans up" 143 RUN_ID=r5 FAKE_APPLY_SLEEP=30
check 'grep -q "terraform project-b interrupted" "$FAKE_CALLS"' "the running apply must be interrupted"
check '[ "$(remaining)" = "" ]' "no run instances may remain"

if [ "$failures" -ne 0 ]; then
  echo "$failures failure(s)"; exit 1
fi
echo "all e2e safety checks passed"
