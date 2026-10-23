#!/usr/bin/env bash
# check-migration-down-request.sh ENVIRONMENT STEPS CONFIRM MIGRATIONS_PATH
#
# The gate database-migration.yml puts in front of every `migrate ... down N`.
# The `validate` job runs it, and each migrate-* job runs it again right before
# it calls migrate, so a regression in one place cannot reach the database.
#
# A down request passes only when all of these hold:
#
#   1. STEPS is an explicit positive integer. steps=0 (the input default)
#      would run `migrate down -all`.
#   2. STEPS is smaller than the number of migration versions in
#      MIGRATIONS_PATH. golang-migrate does not reject a `down N` larger than
#      what is applied: readDown() rolls back everything that remains and only
#      then returns ErrShortLimit, so steps=99999999 is `down -all` with an
#      error printed after the schema is gone (#68). The cap is applied in
#      every environment, because rolling back ALL migrations is unsupported by
#      this workflow everywhere, not only on prod.
#   3. On prod, CONFIRM is exactly "rollback-prod-<STEPS>". The bare
#      "rollback-prod" confirmed intent to roll back prod but not how much of
#      it, so the operator now retypes the magnitude they are about to drop.
#
# Prints the reason for every failed check and exits 1 if any failed, 0 if the
# request is safe to run.

set -uo pipefail

if [[ $# -ne 4 ]]; then
  echo "usage: $0 ENVIRONMENT STEPS CONFIRM MIGRATIONS_PATH" >&2
  exit 2
fi

ENVIRONMENT="$1"
STEPS="$2"
CONFIRM="$3"
MIGRATIONS_PATH="$4"
ok=true

# count_versions DIR prints how many distinct migration versions golang-migrate's
# file source sees in DIR: files named <digits>_<name>.up.sql or .down.sql,
# counted once per numeric version whether it has one file or both.
count_versions() {
  find "$1" -maxdepth 1 -type f \( -name '[0-9]*_*.up.sql' -o -name '[0-9]*_*.down.sql' \) 2>/dev/null |
    awk -F/ '{ v = $NF; sub(/_.*/, "", v); if (v ~ /^[0-9]+$/) seen[v + 0] = 1 }
             END { n = 0; for (v in seen) n++; print n }'
}

if ! [[ "$STEPS" =~ ^[1-9][0-9]*$ ]]; then
  echo "❌ direction=down requires an explicit positive 'steps' value (got '$STEPS')."
  echo "Rolling back ALL migrations at once is not supported by this workflow."
  ok=false
fi

TOTAL=$(count_versions "$MIGRATIONS_PATH")
if [[ "$TOTAL" -eq 0 ]]; then
  echo "❌ No migration versions found under '$MIGRATIONS_PATH'; cannot bound a rollback."
  ok=false
elif [[ "$ok" == "true" ]]; then
  # Compare lengths first: a STEPS too long for a bash integer would wrap in
  # arithmetic (18446744073709551617 evaluates to 1) and slip under the cap.
  if [[ ${#STEPS} -gt ${#TOTAL} ]] || (( 10#$STEPS >= TOTAL )); then
    echo "❌ steps=$STEPS would roll back all $TOTAL migration(s) in this tree."
    echo "golang-migrate treats a down count at or past the applied total as 'roll back everything'."
    echo "The largest accepted value is $((TOTAL - 1)); rolling back ALL migrations is not supported by this workflow."
    ok=false
  fi
fi

if [[ "$ENVIRONMENT" == "prod" ]]; then
  echo "⚠️ WARNING: Attempting to rollback migrations on PRODUCTION"
  echo "This operation is destructive and may cause data loss!"
  if [[ "$CONFIRM" != "rollback-prod-$STEPS" ]]; then
    echo "❌ Production rollback requires typing 'rollback-prod-$STEPS' (the step count you are rolling back) in the 'confirm' input."
    ok=false
  fi
fi

if [[ "$ok" != "true" ]]; then
  exit 1
fi
