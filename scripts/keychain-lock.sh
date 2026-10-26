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
# How long a takeover marker may sit before the next generation takes over
# from it. A takeover takes milliseconds.
MARK_STALE="${HULL_KEYCHAIN_LOCK_MARK_STALE:-120}"
OWNER="${GITHUB_RUN_ID:-local}.${GITHUB_RUN_ATTEMPT:-0}.${GITHUB_JOB:-job}"

holder() { cat "$LOCK/owner" 2> /dev/null || echo "unknown"; }

# Seconds since a path last changed, or nothing when it is gone.
since() {
  m="$(stat -f %m "$1" 2> /dev/null)" || return 0
  echo $(( $(date +%s) - m ))
}

# The lock's age: from the owner file, or from the directory when the holder
# died before writing one. Nothing when no lock exists.
age() {
  since "$LOCK/owner" | grep . || since "$LOCK"
}

# The identity of a lock directory: its inode, its birth time and its owner. A
# lock that is removed and made again has a new one. Nothing when it is gone.
ident() {
  i="$(stat -f '%i.%B' "$1" 2> /dev/null)" || return 0
  o="$(cat "$1/owner" 2> /dev/null | tr -c 'A-Za-z0-9._-' '_')" || true
  echo "$i.${o:-none}"
}

# Take over the stale lock whose identity is $1, and return 0 once it is gone.
#
# Only the waiter that creates the takeover marker for this identity acts, so
# two waiters that saw the same stale lock never both act. No marker is ever
# cleared to make room: a marker names one lock, and a lock made again has
# another identity. A marker older than MARK_STALE was left by a waiter killed
# here, and the next generation of the marker takes over from it.
#
# The winner checks that $LOCK is still the lock it judged stale, moves it
# aside with one rename, and only then removes it. A removal that fails, or a
# job cancelled after the rename, leaves a renamed directory that blocks
# nobody. If the moved directory turns out to be another lock, which takes a
# winner stalled past MARK_STALE between two commands, it is put back.
take_over() {
  id="$1"
  [ -n "$id" ] || return 1
  gen=1
  while ! mkdir "$LOCK.takeover.$id.$gen" 2> /dev/null; do
    t="$(since "$LOCK.takeover.$id.$gen")"
    # A live waiter holds this generation, or the marker cannot be made.
    if [ -z "$t" ] || [ "$t" -le "$MARK_STALE" ] || [ "$gen" -ge 5 ]; then
      return 1
    fi
    gen=$(( gen + 1 ))
  done
  [ "$(ident "$LOCK")" = "$id" ] || return 1
  a="$(age)"
  if [ -z "$a" ] || [ "$a" -le "$STALE" ]; then
    return 1
  fi
  dead="$LOCK.dead.$id.$gen"
  if ! mv "$LOCK" "$dead" 2> /dev/null; then
    echo "keychain-lock: cannot move aside the lock held for ${a}s by $(holder):" \
      "$(ls -ld "$LOCK" 2> /dev/null || echo gone)" >&2
    return 1
  fi
  if [ "$(ident "$dead")" != "$id" ]; then
    echo "keychain-lock: moved a live lock aside by mistake; putting it back" >&2
    [ -e "$LOCK" ] || mv "$dead" "$LOCK" 2> /dev/null || true
    return 1
  fi
  echo "keychain-lock: took over a lock held for ${a}s by $(cat "$dead/owner" 2> /dev/null || echo unknown)" >&2
  rm -rf "$dead" 2> /dev/null || true
}

# Markers and moved-aside locks are left behind on purpose (see take_over).
# One older than twice STALE names a lock that is long gone.
prune() {
  find "$(dirname "$LOCK")" -maxdepth 1 \
    \( -name "$(basename "$LOCK").takeover.*" -o -name "$(basename "$LOCK").dead.*" \) \
    -mmin +$(( STALE * 2 / 60 )) -exec rm -rf {} + 2> /dev/null || true
}

case "${1:-}" in
  acquire)
    prune
    waited=0
    until mkdir "$LOCK" 2> /dev/null; do
      a="$(age)"
      if [ -n "$a" ] && [ "$a" -gt "$STALE" ] && take_over "$(ident "$LOCK")"; then
        # The stale lock is gone: try for it again at once.
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
