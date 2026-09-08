#!/usr/bin/env python3
# Terminal-handling regression harness (issue #12): boots a foreground run on
# a PTY, delivers the scenario signal, and asserts the parent exits, no VMM
# is left behind, no output lands on the tty after exit (the "mixed
# terminal" symptom), and the instance state reads stopped.
#
# Usage: HULL_BIN=dist/hull_arm64 \
#        python3 test/pty-terminal-test.py <vz|qemu> <name> <intr|double|term|type>
import os, pty, sys, time, subprocess, select, fcntl, termios, signal as sig

BIN = os.environ.get("HULL_BIN", "dist/hull_arm64")
STORE = os.environ.get("HULL_STORE_DIR", "")
GLOBAL_ARGS = (["--store-dir", STORE] if STORE else [])
hv, name, mode = sys.argv[1], sys.argv[2], sys.argv[3]
failures = []  # mode: intr | double | term

master, slave = pty.openpty()
# The type scenario needs the image's default interactive shell (it types
# into it and checks the echo); the signal scenarios use a quiet workload.
workload = [] if mode == "type" else ["/bin/sleep", "300"]
t_spawn = time.time()
child = subprocess.Popen(
    [BIN] + GLOBAL_ARGS + ["run", "--hypervisor", hv, "--net", "none", "--stop-grace", "4", "--name", name, "--",
     "harbor.nbfc.io/nubificus/urunc-ubuntu-vz:aarch64"] + workload,
    stdin=slave, stdout=slave, stderr=slave,
    preexec_fn=lambda: (os.setsid(), fcntl.ioctl(0, termios.TIOCSCTTY, 0)))
os.close(slave)

all_output = b""
# Boot timing, printed and never asserted on: when hull reported the VMM
# process spawned (its own "VMM started" line, which comes after any image
# pull) and when each boot marker first showed up. The numbers are what
# README quotes for boot time, so they come from here instead of by hand.
VMM_MARKER = b"VMM started (PID"
t_vmm = None

def drain(sec):
    global all_output
    out = b""
    end = time.time() + sec
    while time.time() < end:
        r,_,_ = select.select([master], [], [], 0.5)
        if r:
            try: out += os.read(master, 4096)
            except OSError: break
    all_output += out
    return out

def die(reason):
    print(f"FAIL: {reason}")
    print("---- console transcript ----")
    sys.stdout.buffer.write(all_output[-4000:]); sys.stdout.flush()
    print("\n----------------------------")
    subprocess.run([BIN] + GLOBAL_ARGS + ["stop", name], capture_output=True)
    subprocess.run([BIN] + GLOBAL_ARGS + ["rm", name], capture_output=True)
    if child.poll() is None:
        child.kill()
    sys.exit(1)

def write_pty(data):
    try:
        os.write(master, data)
    except OSError as e:
        die(f"pty write failed ({e}); run process likely died")

def wait_boot(marker, timeout):
    """Wait for a boot marker, tolerating slow image pulls; the child dying
    during boot is a hard failure with the transcript dumped. Returns the
    time the marker was first seen. Reads are checked one at a time so the
    timestamp is the read that carried the marker, not a one-second tick."""
    global all_output, t_vmm
    end = time.time() + timeout
    while time.time() < end:
        r,_,_ = select.select([master], [], [], 0.1)
        if r:
            try: all_output += os.read(master, 4096)
            except OSError: pass
        now = time.time()
        if t_vmm is None and VMM_MARKER in all_output:
            t_vmm = now
        if marker in all_output:
            return now
        if child.poll() is not None:
            die(f"run exited during boot (rc={child.poll()})")
    die(f"boot marker {marker!r} not seen within {timeout}s")

def print_boot_time(label, t_marker):
    since = t_vmm if t_vmm is not None else t_spawn
    origin = "" if t_vmm is not None else ", from hull spawn: no VMM-started line seen"
    print(f"boot: {label} {t_marker - since:.2f}s (backend {hv}{origin})")

BOOT_TIMEOUT = int(os.environ.get("HULL_TEST_BOOT_TIMEOUT", "180"))
# 'Run /.' is the init-wrapper exec line, common to all rootfs modes; the
# interactive default command additionally reaches a shell prompt.
t_init = wait_boot(b"Run /.", BOOT_TIMEOUT)
print_boot_time("kernel-to-init", t_init)
print(f"boot: hull-spawn-to-init {t_init - t_spawn:.2f}s (backend {hv}, includes any image pull)")
if mode == "type":
    t_shell = wait_boot(b"root@", 30)
    print_boot_time("kernel-to-shell", t_shell)
drain(2)  # settle
if mode == "type":
    # Regression for the vz double-echo: with the host tty raw, a typed
    # line must appear exactly once (guest echo), like the QEMU backend.
    write_pty(b"echo hello\n")
    typed = drain(3)
    n = typed.count(b"echo hello")
    print(f"typed line appears {n}x {'OK' if n == 1 else 'DOUBLE-ECHO BUG'}")
    if n != 1:
        failures.append(f"typed line echoed {n}x (want 1)")
    write_pty(b"\x03")
elif mode == "intr":
    write_pty(b"\x03")
elif mode == "double":
    write_pty(b"\x03"); time.sleep(1.0); write_pty(b"\x03")
elif mode == "term":
    child.send_signal(sig.SIGTERM)

deadline = time.time() + 20
while time.time() < deadline and child.poll() is None:
    drain(0.5)
rc = child.poll()
print(f"parent exited: {rc is not None} (rc={rc})")
if rc is None:
    failures.append("parent still running")
residual = drain(3)
print(f"residual tty output after exit: {len(residual)} bytes {('OK' if not residual else 'MIXING! ' + repr(residual[:100]))}")
if residual:
    failures.append(f"{len(residual)} bytes on the tty after exit")

# leftover check scoped to THIS instance (the runner may host other VMs).
# Match on the executable name (comm) to avoid false positives from shell
# command lines that merely mention the strings.
orphans = []
psout = subprocess.run(["ps", "-eo", "pid=,comm="], capture_output=True, text=True).stdout
for l in psout.splitlines():
    pid, _, comm = l.strip().partition(" ")
    exe = os.path.basename(comm.strip())
    if exe not in ("vz-runner", "qemu-system-aarch64"):
        continue
    argv = subprocess.run(["ps", "-p", pid, "-o", "command="], capture_output=True, text=True).stdout
    if name in argv:
        orphans.append(argv.strip()[:120])
print("leftover VMMs for this instance:", len(orphans))
if orphans:
    failures.append("VMM process left behind")

ps = subprocess.run([BIN] + GLOBAL_ARGS + ["ps"], capture_output=True, text=True)
# Read the exit code, not just stdout. This check fails closed, so a broken
# `hull ps` already produced a failure -- but it reported "instance state is
# not stopped", which sends the reader after the guest instead of the CLI.
if ps.returncode != 0:
    failures.append(f"hull ps exited {ps.returncode}, so the instance state was never read")
st = ps.stdout
state_ok = False
for line in st.splitlines():
    if name in line:
        print("STATE:", " ".join(line.split()[:2]))
        state_ok = "stopped" in line
if not state_ok:
    failures.append("instance state is not stopped")

subprocess.run([BIN] + GLOBAL_ARGS + ["stop", name], capture_output=True)
subprocess.run([BIN] + GLOBAL_ARGS + ["rm", name], capture_output=True)
if child.poll() is None: child.kill()
if failures:
    print("FAIL:", "; ".join(failures)); sys.exit(1)
print("PASS")
