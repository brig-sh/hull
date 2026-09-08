#!/bin/bash
# Self-test for the two VM-boot harnesses: proves each one can actually FAIL,
# using fake `hull` binaries instead of a real VM.
#
# hvi-boot-test.py and pty-jobcontrol-test.py both used to report a clean
# PASS/CLEAN result on inputs that are not clean at all: a guest that prints
# its token and then dies, a `hull ps -a` that fails outright and prints
# nothing, and a `hull` binary that does not even exist. This script pins
# that regression down. It runs each harness against a fake `hull` built to
# reproduce exactly those failures and fails itself if the harness does not
# fail too, then runs each harness against a genuine success to prove the
# fix did not turn either harness into something that always fails.
#
# One case here is not about that regression: case 3 covers a check in
# hvi-boot-test.py that predates the fix and had no coverage at all.
#
# It is cheap because it needs no VM, no hypervisor, no boot assets, no `hvi`
# binary, no built `hull`, no network and no code signing: every case drives
# a small Python stub and the whole run is seconds of CPU. It exercises only
# the harnesses' own verdict logic.
#
# It does need a Darwin/arm64 host. hvi-boot-test.py gates on Apple Silicon
# (`platform.system() != "Darwin" or platform.machine() != "arm64"`) ahead of
# every other check and skips with exit 0 anywhere else, so on another host
# cases 1 to 5 would all be asserting against that skip instead of against a
# verdict. Measured by shadowing `platform` with a stub reporting
# Linux/x86_64: case 1's invocation then prints
# "SKIP: hvi needs Apple Silicon, this is Linux/x86_64" and exits 0, where
# case 1 requires exit 1.
#
# Run after any change to either harness.
set -uo pipefail   # not -e: nonzero exits from the harnesses are expected data, not script errors

# A caller's shell commonly exports these for the real harnesses (test/README.md's
# own quickstart exports the first two). Our fake `hull` dispatches on argv[1] to
# decide run/ps/stop/rm; a stray --store-dir would land there instead and silently
# no-op every case. Each case below sets exactly what it needs per invocation.
#
# HULL_TEST_BOOT_TIMEOUT is unset for a smaller reason: pty-jobcontrol-test.py
# sizes its wait for the boot marker from it. It does not change any verdict
# here -- measured on the version of this script that predates this line, with
# it exported as 0, every case still reached the same verdict, because each of
# cases 6 to 8 settles as soon as the fake shell is reaped or the marker
# arrives. What it changes is runtime: that run took 43s against 21s, since
# collapsing the wait loop pushes each pty case onto the fixed drain that
# follows it. Runtime here should not depend on the caller's shell.
unset HULL_BIN HULL_STORE_DIR HULL_BOOT_ASSETS HULL_HVI_IMAGE HULL_TEST_BOOT_TIMEOUT

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

pass=0
fail=0

# expect_exit <label> <expected-exit> <actual-exit>
expect_exit() {
    local label=$1 expected=$2 actual=$3
    if [ "$actual" -ne "$expected" ]; then
        echo "FAIL  $label: expected exit $expected, got $actual"
        fail=$((fail + 1))
    else
        echo "OK    $label (exit $actual)"
        pass=$((pass + 1))
    fi
}

# expect_contains <label> <haystack> <needle>
#
# Several cases below match on a harness's own failure text. That is
# deliberate and not string-matching for its own sake: every one of these
# cases already asserts the exit code, and an exit code alone cannot tell
# "failed for the reason under test" from "failed somewhere earlier". The
# needle is always the one phrase that separates the two.
expect_contains() {
    local label=$1 haystack=$2 needle=$3
    if printf '%s' "$haystack" | grep -qF -- "$needle"; then
        echo "OK    $label (found '$needle')"
        pass=$((pass + 1))
    else
        echo "FAIL  $label: expected to find '$needle'"
        fail=$((fail + 1))
    fi
}

# --- a fake hull for hvi-boot-test.py, behavior driven entirely by env vars ---
cat > "$WORK/hull" <<'PYEOF'
#!/usr/bin/env python3
import os, sys
sub = sys.argv[1] if len(sys.argv) > 1 else ""
if sub == "run":
    token = os.environ.get("FAKE_TOKEN", "")
    if token:
        print(token)
    sys.exit(int(os.environ.get("FAKE_RUN_EXIT", "0")))
elif sub == "ps":
    out = os.environ.get("FAKE_PS_OUTPUT", "")
    if out:
        print(out)
    sys.exit(int(os.environ.get("FAKE_PS_EXIT", "0")))
else:
    sys.exit(0)  # stop / rm cleanup calls: always succeed quietly
PYEOF
chmod +x "$WORK/hull"
touch "$WORK/hvi"; chmod +x "$WORK/hvi"
mkdir -p "$WORK/assets"
touch "$WORK/assets/Image" "$WORK/assets/container-initrd"

echo "== hvi-boot-test.py =="

# Case 1 (required): the token reaches the console, then `hull run` exits
# nonzero -- a guest that printed and then died. Before the fix this read
# run.stdout for the token and never looked at run.returncode, so it PASSED.
out=$(HULL_BIN="$WORK/hull" HULL_BOOT_ASSETS="$WORK/assets" \
      FAKE_TOKEN="hvi-booted-c1" FAKE_RUN_EXIT=1 \
      python3 "$HERE/hvi-boot-test.py" c1 2>&1); rc=$?
expect_exit "case 1: entrypoint ran, hull run exited nonzero" 1 "$rc"
# Not just "FAIL": exit 1 already implies that, and the token check that runs
# before this one also exits 1. Only the exit code in the message proves the
# harness got as far as reading run.returncode.
expect_contains "case 1: names hull's exit code" "$out" "hull run exited 1"

# Case 2 (required): the token reaches the console and hull run exits 0,
# but `hull ps -a` itself exits nonzero and prints nothing. Before the fix
# ps.returncode was never read, so the empty output looked like "nothing
# still running" -- a clean instance indistinguishable from a broken probe.
out=$(HULL_BIN="$WORK/hull" HULL_BOOT_ASSETS="$WORK/assets" \
      FAKE_TOKEN="hvi-booted-c2" FAKE_RUN_EXIT=0 FAKE_PS_EXIT=1 \
      python3 "$HERE/hvi-boot-test.py" c2 2>&1); rc=$?
expect_exit "case 2: entrypoint ran, hull ps -a exited nonzero" 1 "$rc"
expect_contains "case 2: names the ps failure" "$out" "ps -a exited"

# Case 3: `hull ps -a` succeeds and reports the instance still running after
# its command exited -- a VMM nobody is watching, which the next run would
# inherit. This branch is not one of the ones fixed above; it predates the
# fix and simply had no coverage, and cases 1 and 2 cannot reach it because
# neither gets as far as a `ps` table to scan. It guards the scan against
# being dropped or loosened: measured with FAKE_PS_OUTPUT removed from this
# same invocation, the harness prints PASS and exits 0, so this case is the
# only thing holding that branch in place. The fake table carries a real
# header row ("ID" in column 1), which also shows the scan does not mistake
# the header for an instance.
out=$(HULL_BIN="$WORK/hull" HULL_BOOT_ASSETS="$WORK/assets" \
      FAKE_TOKEN="hvi-booted-c3" FAKE_RUN_EXIT=0 FAKE_PS_EXIT=0 \
      FAKE_PS_OUTPUT=$'ID  STATUS  EXIT  PID  IP  CREATED\nc3  running  -  4242  -  1 second ago' \
      python3 "$HERE/hvi-boot-test.py" c3 2>&1); rc=$?
expect_exit "case 3: ps -a still lists the instance as running" 1 "$rc"
expect_contains "case 3: names the instance left running" "$out" "c3 is still running"

# Positive control: a genuine success must still pass. Without this, a
# harness rewritten to always fail would pass cases 1 to 3 trivially.
out=$(HULL_BIN="$WORK/hull" HULL_BOOT_ASSETS="$WORK/assets" \
      FAKE_TOKEN="hvi-booted-c4" FAKE_RUN_EXIT=0 FAKE_PS_EXIT=0 \
      python3 "$HERE/hvi-boot-test.py" c4 2>&1); rc=$?
expect_exit "case 4: genuine clean boot still passes" 0 "$rc"
expect_contains "case 4: reports PASS" "$out" "PASS"

# Case 5 (required): a genuinely missing prerequisite must still SKIP (exit
# 0) with a named reason, not fail. Proves the fixes above did not turn a
# legitimate skip into a failure.
out=$(HULL_BIN="$WORK/does-not-exist" python3 "$HERE/hvi-boot-test.py" c5 2>&1); rc=$?
expect_exit "case 5: missing hull binary still skips" 0 "$rc"
expect_contains "case 5: names the missing prerequisite" "$out" "SKIP:"

echo
echo "== pty-jobcontrol-test.py =="

# Case 6 (required): a `hull` binary that does not exist. The old harness
# passed this. Its whole verdict was
#     verdict = 'SUSPENDED-BUG' if os.WEXITSTATUS(st) == 3 else 'CLEAN'
# so CLEAN was the default for any exit status other than 3 and the console
# was never consulted at all. execv fails, the fake shell prints a
# FileNotFoundError traceback to the pty and exits 0, 0 is not 3, so: CLEAN.
# Measured by running the pre-fix harness, the version this round replaced:
#     $ python3 pty-jobcontrol-test.py /nonexistent/hull vz probe-x
#     console tail: b''
#     verdict: CLEAN
#     $? = 0
# That empty tail is an artifact and not the mechanism. The old code called
# drain(9), threw the result away and printed a tail taken only from the
# later drain(12), by which time the traceback had long been read off the
# pty. The console was not empty, and emptiness was never what produced
# CLEAN. The fix does not test for emptiness either: it requires positive
# boot evidence, the 'Run /.' marker, before CLEAN is allowed, and reports
# NO-BOOT-EVIDENCE without it. The same command against today's harness
# prints a tail holding the FileNotFoundError line and "FAKESHELL: job ended
# cleanly", then "verdict: NO-BOOT-EVIDENCE", and exits 1.
out=$(timeout 60 python3 "$HERE/pty-jobcontrol-test.py" "$WORK/does-not-exist-hull" vz selftest-noboot 2>&1); rc=$?
expect_exit "case 6: nonexistent hull binary must not pass" 1 "$rc"
# Assert the verdict positively. "not CLEAN" would also be satisfied by a
# harness that hung until `timeout` killed it (exit 124, no output at all),
# which is the same vacuous pass this script exists to catch.
expect_contains "case 6: reports NO-BOOT-EVIDENCE" "$out" "verdict: NO-BOOT-EVIDENCE"

# Positive control: a fake hull that actually writes the boot marker hull's
# own boot sequence writes ('Run /.', the same marker pty-terminal-test.py
# waits on) and then exits cleanly on the test's ^C must still verdict CLEAN.
# Without this, a harness rewritten to never say CLEAN would pass case 6
# trivially.
cat > "$WORK/fake-boot-hull" <<'PYEOF'
#!/usr/bin/env python3
import sys, time
print("Run /.")
sys.stdout.flush()
try:
    time.sleep(30)
except KeyboardInterrupt:
    sys.exit(0)
PYEOF
chmod +x "$WORK/fake-boot-hull"
out=$(timeout 60 python3 "$HERE/pty-jobcontrol-test.py" "$WORK/fake-boot-hull" vz selftest-clean 2>&1); rc=$?
expect_exit "case 7: genuine boot still verdicts CLEAN" 0 "$rc"
expect_contains "case 7: reports CLEAN" "$out" "verdict: CLEAN"

# Case 8: a job that gets SIGTTOU-suspended before it ever writes the boot
# marker must still report SUSPENDED-BUG, not NO-BOOT-EVIDENCE. Pins the
# verdict precedence (stopped-signal check runs before the marker check) so
# a future reorder of that branch cannot silently swap the two.
cat > "$WORK/fake-stop-hull" <<'PYEOF'
#!/usr/bin/env python3
import os, signal, time
os.kill(os.getpid(), signal.SIGTTOU)
time.sleep(30)
PYEOF
chmod +x "$WORK/fake-stop-hull"
out=$(timeout 60 python3 "$HERE/pty-jobcontrol-test.py" "$WORK/fake-stop-hull" vz selftest-stop 2>&1); rc=$?
expect_exit "case 8: SIGTTOU before any boot marker" 1 "$rc"
expect_contains "case 8: reports SUSPENDED-BUG" "$out" "verdict: SUSPENDED-BUG"

echo
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
