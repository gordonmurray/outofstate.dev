#!/usr/bin/env bash
# End-to-end test for oos against a real AWS sandbox account (PLAN.md §10).
#
# Creates three db.t4g.micro RDS instances (billable), untracks one, checks
# oos reports exactly that one, imports it back, checks oos is clean, and
# always destroys everything, even when a step fails.
#
# Runs inside the e2e image (make e2e). Needs AWS_PROFILE and AWS_REGION, and
# E2E_CONFIRM=yes. The account and region must have no other RDS instances.
set -euo pipefail

: "${AWS_PROFILE:?set AWS_PROFILE}"
: "${AWS_REGION:?set AWS_REGION}"
if [ "${E2E_CONFIRM:-}" != "yes" ]; then
  echo "This creates 3 billable db.t4g.micro RDS instances in ${AWS_REGION} for about 20 minutes." >&2
  echo "Re-run with E2E_CONFIRM=yes to go ahead." >&2
  exit 2
fi

run_id="${RUN_ID:-$(date +%Y%m%d%H%M%S)}"
oos=/oos/oos
work="$(mktemp -d)"
cp -r /e2e/project-a /e2e/project-b "$work/"
a="$work/project-a"
b="$work/project-b"
export TF_VAR_run_id="$run_id" TF_IN_AUTOMATION=1 TF_INPUT=0

log() { printf '\n== %s\n' "$*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

cleanup() {
  local status=$?
  log "cleanup (run ${run_id})"
  terraform -chdir="$a" destroy -auto-approve >/dev/null 2>&1 || true
  terraform -chdir="$b" destroy -auto-approve >/dev/null 2>&1 || true
  # Anything still tagged with this run (for example b2 if the import failed).
  for id in $(aws rds describe-db-instances --query "DBInstances[?TagList[?Key=='oos-e2e' && Value=='${run_id}']].DBInstanceIdentifier" --output text); do
    echo "deleting leftover ${id}"
    aws rds delete-db-instance --db-instance-identifier "$id" --skip-final-snapshot >/dev/null || true
  done
  for id in $(aws rds describe-db-instances --query "DBInstances[?TagList[?Key=='oos-e2e' && Value=='${run_id}']].DBInstanceIdentifier" --output text); do
    aws rds wait db-instance-deleted --db-instance-identifier "$id" || true
  done
  rm -rf "$work"
  if [ "$status" -eq 0 ]; then echo "PASS"; fi
  exit "$status"
}
trap cleanup EXIT

# Run oos, keep the JSON report, and check the exit code.
run_oos() {
  local want=$1; shift
  set +e
  "$oos" --region "$AWS_REGION" --format json --output "$work/report.json" "$@"
  local got=$?
  set -e
  [ "$got" -eq "$want" ] || fail "oos exit code ${got}, want ${want} (args: $*)"
}

unmatched() { jq -r '[.findings[] | select(.status=="no_match") | .identifier] | sort | join(",")' "$work/report.json"; }

log "checking the region has no other RDS instances"
existing=$(aws rds describe-db-instances --query 'length(DBInstances)' --output text)
[ "$existing" = "0" ] || fail "${existing} RDS instance(s) already exist in ${AWS_REGION}; use an empty sandbox region"

log "creating instances (run ${run_id})"
terraform -chdir="$a" init -input=false >/dev/null
terraform -chdir="$b" init -input=false >/dev/null
terraform -chdir="$a" apply -auto-approve >/dev/null &
pid_a=$!
terraform -chdir="$b" apply -auto-approve >/dev/null &
pid_b=$!
wait "$pid_a" || fail "apply A"
wait "$pid_b" || fail "apply B"

log "untracking b2"
terraform -chdir="$b" state rm aws_db_instance.b2 >/dev/null

log "oos with A and B: expect only b2 unmatched, exit 2"
run_oos 2 --source "$a" --source "$b"
[ "$(unmatched)" = "oos-e2e-${run_id}-b2" ] || fail "unmatched = $(unmatched)"

log "oos with A only: expect b1 and b2 unmatched"
run_oos 2 --source "$a"
[ "$(unmatched)" = "oos-e2e-${run_id}-b1,oos-e2e-${run_id}-b2" ] || fail "unmatched = $(unmatched)"

log "secrets: the database password must not appear in any report"
password=$(terraform -chdir="$a" state pull | jq -r '.resources[] | select(.type=="random_password") | .instances[0].attributes.result')
"$oos" --region "$AWS_REGION" --source "$a" --source "$b" --output "$work/report.txt" || true
if grep -qF "$password" "$work/report.json" "$work/report.txt"; then fail "password found in a report"; fi

log "importing b2 back"
terraform -chdir="$b" import aws_db_instance.b2 "oos-e2e-${run_id}-b2" >/dev/null

log "oos with A and B: expect everything tracked, exit 0"
run_oos 0 --source "$a" --source "$b"
[ "$(unmatched)" = "" ] || fail "unmatched = $(unmatched)"
