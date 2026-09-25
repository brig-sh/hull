#!/bin/sh
# One signing job at a time on the shared Mac.
#
# Every self-hosted runner here is the same physical Mac, and a signing job
# replaces the user keychain search list with its own ephemeral keychain, then
# puts the login keychain back when it is done. Two of them at once is
# errSecInternalComponent, or one job's cleanup dropping the other's keychain
# mid-sign.
#
# A GitHub concurrency group cannot carry this. A group keeps one running and
# one pending run, and a newer pending run cancels the older one, so the more
# workflows share a group the more often a pending release is cancelled by an
# unrelated merge. This lock lives on the Mac instead: a job waits for it, and
# nothing is ever cancelled.
#
#   keychain-lock.sh acquire   before the keychain is created
#   keychain-lock.sh release   at the end of the always() cleanup
#
# mkdir is the atomic step. A lock older than HULL_KEYCHAIN_LOCK_STALE seconds
# belongs to a job that died without its cleanup, and is taken over. The owner
# file is written once, so its age is how long the lock has been held, not
# whether its holder is alive. That is why every job that takes the lock sets
# timeout-minutes below STALE (75 against 90): a lock older than STALE cannot
# belong to a running job. WAIT is below that timeout, so a waiter reports
# who holds the lock before its own job is killed.
set -eu

LOCK="${HULL_KEYCHAIN_LOCK:-/Users/Shared/hull-keychain.lock}"
STALE="${HULL_KEYCHAIN_LOCK_STALE:-5400}"
WAIT="${HULL_KEYCHAIN_LOCK_WAIT:-3600}"
POLL="${HULL_KEYCHAIN_LOCK_POLL:-10}"
OWNER="${GITHUB_RUN_ID:-local}.${GITHUB_RUN_ATTEMPT:-0}.${GITHUB_JOB:-job}"

holder() { cat "$LOCK/owner" 2> /dev/null || echo "unknown"; }

# The lock's age, from the owner file. Empty while the winner of a mkdir has
# not written it yet, and empty when a release removed it between the test and
# the stat. Neither is stale.
age() {
  m="$(stat -f %m "$LOCK/owner" 2> /dev/null)" || return 0
  echo $(( $(date +%s) - m ))
}

case "${1:-}" in
  acquire)
    waited=0
    until mkdir "$LOCK" 2> /dev/null; do
      a="$(age)"
      if [ -n "$a" ] && [ "$a" -gt "$STALE" ]; then
        echo "keychain-lock: taking over a lock held for ${a}s by $(holder)" >&2
        rm -rf "$LOCK"
        continue
      fi
      if [ "$waited" -ge "$WAIT" ]; then
        echo "keychain-lock: gave up after ${WAIT}s, held by $(holder)" >&2
        exit 1
      fi
      [ $(( waited % 60 )) -ne 0 ] \
        || echo "keychain-lock: waiting, held by $(holder)" >&2
      sleep "$POLL"
      waited=$(( waited + POLL ))
    done
    echo "$OWNER" > "$LOCK/owner"
    echo "keychain-lock: acquired by $OWNER" >&2
    ;;
  release)
    # Only this job's own lock. A lock taken over as stale belongs to someone
    # else by now, and removing it would let a third job in beside them.
    if [ "$(holder)" = "$OWNER" ]; then
      rm -rf "$LOCK"
      echo "keychain-lock: released by $OWNER" >&2
    fi
    ;;
  *)
    echo "usage: keychain-lock.sh acquire|release" >&2
    exit 2
    ;;
esac
