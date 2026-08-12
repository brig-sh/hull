#!/usr/bin/env python3
# Checkpoint/restore e2e harness: boots a foreground Vz run on a PTY, starts
# a tick counter in the guest shell, checkpoints the running VM (SIGUSR1
# round trip via `urunc-macos checkpoint`), lets it keep ticking, stops it,
# restores from the checkpoint, and asserts the counter CONTINUES from the
# checkpointed value instead of restarting — i.e. guest memory state really
# came back.
#
# Usage: URUNC_MACOS_BIN=dist/urunc-macos_arm64 \
#        python3 test/pty-checkpoint-test.py <name>
import os, pty, re, sys, time, subprocess, select, fcntl, termios, signal as sig

BIN = os.environ.get("URUNC_MACOS_BIN", "dist/urunc-macos_arm64")
STORE = os.environ.get("URUNC_STORE_DIR", "")
GLOBAL_ARGS = (["--store-dir", STORE] if STORE else [])
IMAGE = os.environ.get("URUNC_TEST_IMAGE", "harbor.nbfc.io/nubificus/urunc-ubuntu-vz:aarch64")
BOOT_TIMEOUT = int(os.environ.get("URUNC_TEST_BOOT_TIMEOUT", "180"))
name = sys.argv[1] if len(sys.argv) > 1 else "ckpt-e2e"

child = None
master = None
all_output = b""


def cleanup():
    subprocess.run([BIN] + GLOBAL_ARGS + ["stop", name], capture_output=True)
    subprocess.run([BIN] + GLOBAL_ARGS + ["rm", name], capture_output=True)
    if child is not None and child.poll() is None:
        child.kill()


def die(reason):
    print(f"FAIL: {reason}")
    print("---- console transcript ----")
    sys.stdout.buffer.write(all_output[-4000:]); sys.stdout.flush()
    print("\n----------------------------")
    cleanup()
    sys.exit(1)


def spawn(args):
    """Start the CLI on a fresh PTY as a session leader owning it."""
    global child, master, all_output
    all_output = b""
    master, slave = pty.openpty()
    child = subprocess.Popen(
        [BIN] + GLOBAL_ARGS + args,
        stdin=slave, stdout=slave, stderr=slave,
        preexec_fn=lambda: (os.setsid(), fcntl.ioctl(0, termios.TIOCSCTTY, 0)))
    os.close(slave)


def drain(sec):
    global all_output
    out = b""
    end = time.time() + sec
    while time.time() < end:
        r, _, _ = select.select([master], [], [], 0.5)
        if r:
            try:
                out += os.read(master, 4096)
            except OSError:
                break
    all_output += out
    return out


def wait_output(marker, timeout, what=""):
    end = time.time() + timeout
    while time.time() < end:
        drain(1)
        if marker in all_output:
            return
        if child.poll() is not None:
            die(f"process exited waiting for {what or marker!r} (rc={child.poll()})")
    die(f"{what or marker!r} not seen within {timeout}s")


def ticks_seen():
    # Terminate on a non-digit so a tick split across reads is not
    # mis-parsed; guest lines may end \r\n or \r\r\n.
    return [int(m) for m in re.findall(rb"TICK (\d+)[\r\n]", all_output)]


def stop_foreground_run():
    child.send_signal(sig.SIGTERM)
    deadline = time.time() + 30
    while time.time() < deadline and child.poll() is None:
        drain(0.5)
    if child.poll() is None:
        die("run did not exit after SIGTERM")


# --- 1. boot and start a tick counter in the guest shell -------------------
cleanup()  # a previous aborted run must not squat the instance name
spawn(["run", "--net", "none", "--hypervisor", "vz", "--rootfs-type", "block",
       "--name", name, "--", IMAGE])
wait_output(b"root@", BOOT_TIMEOUT, "shell prompt")
drain(2)
os.write(master, b"i=0; while true; do i=$((i+1)); echo TICK $i; sleep 1; done\n")
wait_output(b"TICK 3", 30, "tick counter")

# --- 2. checkpoint the running VM ------------------------------------------
t0 = time.time()
cp = subprocess.run([BIN] + GLOBAL_ARGS + ["checkpoint", name],
                    capture_output=True, text=True)
if cp.returncode != 0:
    die(f"checkpoint failed: {cp.stdout} {cp.stderr}")
print(cp.stdout.strip())
drain(2)
at_checkpoint = max(ticks_seen())
print(f"checkpoint at ~TICK {at_checkpoint} ({time.time()-t0:.2f}s)")

# --- 3. the VM must resume and keep ticking --------------------------------
wait_output(b"TICK %d" % (at_checkpoint + 2), 30, "post-checkpoint ticks")
print("VM resumed after checkpoint and keeps ticking")

# --- 4. stop, then restore from the checkpoint ------------------------------
stop_foreground_run()
last_before_stop = max(ticks_seen())

t0 = time.time()
spawn(["restore", name])
wait_output(b"TICK", 60, "ticks after restore")
drain(3)
restored = ticks_seen()
if not restored:
    die("no ticks after restore")
first = restored[0]
print(f"restore: first tick {first} after {time.time()-t0:.2f}s "
      f"(checkpointed at ~{at_checkpoint}, stopped at {last_before_stop})")

# The counter lives in guest memory: after restore it must continue from the
# checkpoint (allow the in-flight tick), never restart from 1.
if first < 2:
    die(f"counter restarted (TICK {first}): restore cold-booted instead of resuming")
if not (at_checkpoint - 1 <= first <= at_checkpoint + 3):
    die(f"first tick after restore is {first}, expected ~{at_checkpoint}: state mismatch")

# --- 5. teardown -------------------------------------------------------------
stop_foreground_run()
cleanup()
print("PASS")
