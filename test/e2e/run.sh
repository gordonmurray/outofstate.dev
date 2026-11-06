#!/usr/bin/env bash
# End-to-end test for oos against a real AWS sandbox account (PLAN.md §10).
#
# Creates three db.t4g.micro RDS instances (billable), untracks one, checks
# oos reports exactly that one, imports it back, checks oos is clean, and
# destroys everything.
#
# Safety rules:
#   - nothing is created unless E2E_CONFIRM=yes and the region has no RDS
#     instances at all;
#   - cleanup only touches instances whose identifier starts with
#     "oos-e2e-<run id>-" AND carry the tag oos-e2e=<run id>;
#   - the run id is validated, and is never interpolated into a query;
#   - both applies always finish before teardown, and INT/TERM are handled;
#   - PASS is printed only if the test passed and cleanup verified that no
#     instance from this run remains;
#   - Terraform works in $ARTIFACTS/<run id> (on the host) from the start, so
#     its state survives anything. That directory is removed only after
#     cleanup is verified; if cleanup fails, the exit code is 3 and the state
#     stays there for recovery.
#
# Needs AWS_PROFILE and AWS_REGION. Paths can be overridden for the self-test.
set -euo pipefail

OOS_BIN="${OOS_BIN:-/oos/oos}"
E2E_DIR="${E2E_DIR:-/e2e}"
ARTIFACTS="${ARTIFACTS:-/artifacts}"

: "${AWS_PROFILE:?set AWS_PROFILE}"
: "${AWS_REGION:?set AWS_REGION}"
if [ "${E2E_CONFIRM:-}" != "yes" ]; then
  echo "This creates 3 billable db.t4g.micro RDS instances in ${AWS_REGION} for about 20 minutes." >&2
  echo "Re-run with E2E_CONFIRM=yes to go ahead." >&2
  exit 2
fi

run_id="${RUN_ID:-$(date +%Y%m%d%H%M%S)}"
if ! [[ "$run_id" =~ ^[a-z0-9]{1,20}$ ]]; then
  echo "RUN_ID must be 1-20 lowercase letters or digits" >&2
  exit 2
fi
prefix="oos-e2e-${run_id}-"

work="$ARTIFACTS/$run_id"
# Claim the run directory atomically: plain mkdir fails if it already exists,
# so two runs with the same id can never share (or delete) each other's state.
if ! mkdir -p "$ARTIFACTS" 2>/dev/null; then
  echo "cannot create ${ARTIFACTS}" >&2
  exit 2
fi
if ! mkdir "$work" 2>/dev/null; then
  echo "${work} already exists or cannot be created; choose another RUN_ID or recover that run first" >&2
  exit 2
fi
if ! cp -r "$E2E_DIR/project-a" "$E2E_DIR/project-b" "$work/"; then
  echo "cannot write Terraform working directories to ${work}" >&2
  exit 2
fi
a="$work/project-a"
b="$work/project-b"
export TF_VAR_run_id="$run_id" TF_IN_AUTOMATION=1 TF_INPUT=0

aws_cleanup=0 # armed only after preflight
apply_pids=()

log() { printf '\n== %s\n' "$*"; }
fail() {
  echo "FAIL: $*" >&2
  exit 1
}

# run_instances lists identifiers of instances that belong to this run:
# both the identifier prefix and the exact tag must match.
run_instances() {
  aws rds describe-db-instances --output json |
    jq -r --arg id "$run_id" --arg prefix "$prefix" '
      .DBInstances[]
      | select(.DBInstanceIdentifier | startswith($prefix))
      | select(any(.TagList[]?; .Key == "oos-e2e" and .Value == $id))
      | .DBInstanceIdentifier'
}

settle_applies() {
  local pid
  for pid in "${apply_pids[@]}"; do
    if kill -0 "$pid" 2>/dev/null; then
      # SIGTERM, not SIGINT: background jobs of a non-interactive shell
      # ignore SIGINT. Terraform treats both as a graceful interrupt and
      # releases its state lock.
      kill -TERM "$pid" 2>/dev/null || true
    fi
  done
  for pid in "${apply_pids[@]}"; do
    wait "$pid" 2>/dev/null || true
  done
  apply_pids=()
}

cleanup() {
  local status=$? cleanup_ok=1 id left
  trap - EXIT INT TERM
  set +e
  settle_applies
  if [ "$aws_cleanup" -eq 1 ]; then
    log "cleanup (run ${run_id})"
    terraform -chdir="$a" destroy -auto-approve >/dev/null 2>&1 || echo "destroy A failed; falling back to tag cleanup" >&2
    terraform -chdir="$b" destroy -auto-approve >/dev/null 2>&1 || echo "destroy B failed; falling back to tag cleanup" >&2
    left=$(run_instances) || cleanup_ok=0
    for id in $left; do
      echo "deleting leftover ${id}"
      aws rds delete-db-instance --db-instance-identifier "$id" --skip-final-snapshot >/dev/null || cleanup_ok=0
    done
    for id in $left; do
      aws rds wait db-instance-deleted --db-instance-identifier "$id" || cleanup_ok=0
    done
    left=$(run_instances) || cleanup_ok=0
    if [ -n "$left" ]; then
      echo "instances from this run still exist: ${left}" >&2
      cleanup_ok=0
    fi
  fi
  if [ "$cleanup_ok" -ne 1 ]; then
    # The state was never copied anywhere: it is still in $work, on the host.
    echo "CLEANUP FAILED. Terraform state kept in ${work}. Delete instances starting with ${prefix} by hand." >&2
    exit 3
  fi
  rm -rf "$work"
  if [ "$status" -eq 0 ]; then
    echo "PASS"
  fi
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# Run oos, keep the JSON report, and check the exit code.
run_oos() {
  local want=$1 got=0
  shift
  "$OOS_BIN" --region "$AWS_REGION" --format json --output "$work/report.json" "$@" || got=$?
  [ "$got" -eq "$want" ] || fail "oos exit code ${got}, want ${want} (args: $*)"
  [ -s "$work/report.json" ] || fail "no JSON report written"
}

unmatched() { jq -r '[.findings[] | select(.status=="no_match") | .identifier] | sort | join(",")' "$work/report.json"; }

log "checking the region has no RDS instances"
existing=$(aws rds describe-db-instances --output json | jq '.DBInstances | length')
[ "$existing" = "0" ] || fail "${existing} RDS instance(s) already exist in ${AWS_REGION}; use an empty sandbox region"

log "creating instances (run ${run_id})"
aws_cleanup=1
terraform -chdir="$a" init -input=false >/dev/null
terraform -chdir="$b" init -input=false >/dev/null
terraform -chdir="$a" apply -auto-approve >/dev/null &
apply_pids+=($!)
terraform -chdir="$b" apply -auto-approve >/dev/null &
apply_pids+=($!)
apply_a=0 apply_b=0
wait "${apply_pids[0]}" || apply_a=$?
wait "${apply_pids[1]}" || apply_b=$?
apply_pids=()
[ "$apply_a" -eq 0 ] || fail "apply A (exit ${apply_a})"
[ "$apply_b" -eq 0 ] || fail "apply B (exit ${apply_b})"

log "untracking b2"
terraform -chdir="$b" state rm aws_db_instance.b2 >/dev/null

log "oos with A and B: expect only b2 unmatched, exit 2"
run_oos 2 --source "$a" --source "$b"
[ "$(unmatched)" = "${prefix}b2" ] || fail "unmatched = $(unmatched)"

log "oos with A only: expect b1 and b2 unmatched"
run_oos 2 --source "$a"
[ "$(unmatched)" = "${prefix}b1,${prefix}b2" ] || fail "unmatched = $(unmatched)"

log "secrets: neither database password may appear in any report"
"$OOS_BIN" --region "$AWS_REGION" --source "$a" --source "$b" --output "$work/report.txt" || true
[ -s "$work/report.txt" ] || fail "no text report written"
for project in "$a" "$b"; do
  password=$(terraform -chdir="$project" state pull | jq -r '.resources[] | select(.type=="random_password") | .instances[0].attributes.result')
  [ -n "$password" ] && [ "$password" != "null" ] || fail "could not read the test password from ${project}"
  found=0
  grep -qF -- "$password" "$work/report.json" "$work/report.txt" || found=$?
  case "$found" in
    0) fail "a password from ${project} appeared in a report" ;;
    1) ;;
    *) fail "grep failed while checking reports" ;;
  esac
done

log "importing b2 back"
terraform -chdir="$b" import aws_db_instance.b2 "${prefix}b2" >/dev/null

log "oos with A and B: expect everything tracked, exit 0"
run_oos 0 --source "$a" --source "$b"
[ "$(unmatched)" = "" ] || fail "unmatched = $(unmatched)"
