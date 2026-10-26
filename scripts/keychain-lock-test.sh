#!/bin/sh
# Tests for keychain-lock.sh, against a lock in a temporary directory. macOS
# only, like the script (stat -f).
#
#   sh scripts/keychain-lock-test.sh
#
# Each check is a condition string that check() evals, so the variables in it
# expand when it runs, not when it is written.
# shellcheck disable=SC2016,SC2034
set -eu

HERE="$(cd "$(dirname "$0")" && pwd)"
LOCKSH="$HERE/keychain-lock.sh"
T="$(mktemp -d)"
trap 'rm -rf "$T"' EXIT
export HULL_KEYCHAIN_LOCK="$T/lock" HULL_KEYCHAIN_LOCK_POLL=1
# The owner names a job from these; a runner sets them, so the checks below
# would compare against its own job instead of the test's.
unset GITHUB_RUN_ATTEMPT GITHUB_JOB
fail=0
check() { if eval "$2"; then echo "ok   $1"; else echo "FAIL $1"; fail=1; fi; }

# worker N HOLD: acquire as job N, hold the lock HOLD seconds inside a critical
# section that records any overlap, then release.
worker() {
  GITHUB_RUN_ID="w$1" sh "$LOCKSH" acquire 2>> "$T/log" || { echo "acquire-failed" >> "$T/res"; return; }
  if mkdir "$T/cs" 2> /dev/null; then
    sleep "$2"
    rmdir "$T/cs"
    echo "in" >> "$T/res"
  else
    echo "overlap" >> "$T/res"
  fi
  GITHUB_RUN_ID="w$1" sh "$LOCKSH" release 2>> "$T/log"
}

# An old lock with no owner file: the case a job killed between the mkdir and
# the owner write leaves behind.
stale_lock() {
  mkdir "$HULL_KEYCHAIN_LOCK"
  touch -t 202001010000 "$HULL_KEYCHAIN_LOCK"
}

reset() { rm -rf "$T"/lock* "$T/res" "$T/log" "$T/cs"; : > "$T/res"; : > "$T/log"; }

# 1. Eight jobs at once: each gets the lock, never two at a time.
reset
for i in 1 2 3 4 5 6 7 8; do worker "$i" 0.2 & done
wait
check "eight jobs, one at a time" '[ "$(grep -c "^in$" "$T/res")" -eq 8 ] && ! grep -q overlap "$T/res"'

# 2. Eight jobs racing for one stale lock: exactly one takes it over, and the
# jobs still get the lock one at a time.
reset
stale_lock
export HULL_KEYCHAIN_LOCK_STALE=60
for i in 1 2 3 4 5 6 7 8; do worker "$i" 0.2 & done
wait
check "stale lock: every job gets it" '[ "$(grep -c "^in$" "$T/res")" -eq 8 ]'
check "stale lock: never two at a time" '! grep -q overlap "$T/res"'
check "stale lock: taken over once" '[ "$(grep -c "took over" "$T/log")" -eq 1 ]'

# 3. A waiter killed inside the takeover left an old marker: the next
# generation takes over from it.
reset
stale_lock
id="$(stat -f '%i.%B' "$HULL_KEYCHAIN_LOCK").none"
mkdir "$HULL_KEYCHAIN_LOCK.takeover.$id.1"
touch -t 202001010000 "$HULL_KEYCHAIN_LOCK.takeover.$id.1"
worker 1 0
check "orphaned marker: the next generation takes over" '[ "$(cat "$T/res")" = in ] && grep -q "took over" "$T/log"'

# 4. A live waiter holds the marker and the lock stays: a waiter sleeps between
# attempts and gives up after WAIT, rather than spinning.
reset
stale_lock
id="$(stat -f '%i.%B' "$HULL_KEYCHAIN_LOCK").none"
mkdir "$HULL_KEYCHAIN_LOCK.takeover.$id.1"
start=$(date +%s)
HULL_KEYCHAIN_LOCK_WAIT=3 GITHUB_RUN_ID=w9 sh "$LOCKSH" acquire 2>> "$T/log" & pid=$!
( sleep 20; kill "$pid" 2> /dev/null ) & guard=$!
rc=0; wait "$pid" || rc=$?
kill "$guard" 2> /dev/null || true
wait "$guard" 2> /dev/null || true
took=$(( $(date +%s) - start ))
check "held marker: gives up after WAIT, does not spin" '[ "$rc" -eq 1 ] && [ "$took" -ge 3 ] && [ "$took" -lt 15 ]'
check "held marker: the lock is left alone" '[ -d "$HULL_KEYCHAIN_LOCK" ]'

# 5. A fresh lock is never taken over, and release leaves another job's lock.
reset
GITHUB_RUN_ID=w1 sh "$LOCKSH" acquire 2>> "$T/log"
HULL_KEYCHAIN_LOCK_WAIT=2 GITHUB_RUN_ID=w2 sh "$LOCKSH" acquire 2>> "$T/log" || true
GITHUB_RUN_ID=w2 sh "$LOCKSH" release 2>> "$T/log"
check "fresh lock: kept by its holder" '[ "$(cat "$HULL_KEYCHAIN_LOCK/owner")" = w1.0.job ]'
GITHUB_RUN_ID=w1 sh "$LOCKSH" release 2>> "$T/log"
check "release: removes its own lock" '[ ! -e "$HULL_KEYCHAIN_LOCK" ]'

exit "$fail"
