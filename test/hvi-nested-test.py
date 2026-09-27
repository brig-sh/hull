#!/usr/bin/env python3
# HVI nested virtualization harness: boots an unmodified OCI image on the hvi
# backend with --nested-virt and asserts the guest kernel came up at EL2 with
# KVM working, then boots the same image without the flag and asserts it came
# up at EL1 without KVM. The second boot is the half that matters most: nested
# virtualization is opt-in, and a guest that got /dev/kvm without asking would
# be a guest running VMs nobody decided it could run.
#
# It SKIPS (exit 0) only when the machine cannot run it: not Apple Silicon, no
# hull or hvi binary, no boot artifacts, or `hull capabilities` reporting that
# hvi answered and the answer was no (`answered: true, supported: false`).
# Any other "not supported" is a FAIL: hvi never answered (missing, hung,
# failed, printed something unreadable, or has no caps probe), which is hull
# or hvi broken and not a host that lacks the feature. A hull whose document
# has no `answered` field fails the same way. With HULL_REQUIRE_NESTED=1 even
# a real "no" fails, for a runner known to have EL2.
#
# Usage: HULL_BIN=dist/hull_arm64 python3 test/hvi-nested-test.py <name>
import json, os, platform, shutil, subprocess, sys, time

BIN = os.environ.get("HULL_BIN", "dist/hull_arm64")
STORE = os.environ.get("HULL_STORE_DIR", "")
GLOBAL_ARGS = ["--store-dir", STORE] if STORE else []
IMAGE = os.environ.get("HULL_HVI_IMAGE", "docker.io/library/ubuntu:latest")
BOOT_TIMEOUT = int(os.environ.get("HULL_TEST_BOOT_TIMEOUT", "180"))
REQUIRE = os.environ.get("HULL_REQUIRE_NESTED") == "1"
base = sys.argv[1] if len(sys.argv) > 1 else "hvi-nested"
NESTED = f"{base}-kvm"
CONTROL = f"{base}-nokvm"


class Failed(Exception):
    pass


def skip(reason):
    print(f"SKIP: {reason}")
    sys.exit(0)


def die(reason, detail=""):
    raise Failed(reason + (f"\n---- output ----\n{detail[-4000:]}" if detail else ""))


def hull(*args, timeout=60, split=False):
    """Run hull. split keeps stderr apart, for output parsed as JSON: a
    logrus warning on stderr must not land in front of the document."""
    try:
        return subprocess.run([BIN, *GLOBAL_ARGS, *args], stdout=subprocess.PIPE,
                              stderr=subprocess.PIPE if split else subprocess.STDOUT,
                              text=True, errors="replace", timeout=timeout)
    except subprocess.TimeoutExpired:
        die(f"hull {' '.join(args)} did not return within {timeout}s")


def cleanup(name):
    # Its own bound, so a wedged stop cannot hang the harness it is
    # cleaning up after.
    for verb in ("stop", "rm"):
        try:
            subprocess.run([BIN, *GLOBAL_ARGS, verb, name], stdout=subprocess.DEVNULL,
                           stderr=subprocess.DEVNULL, timeout=120)
        except subprocess.TimeoutExpired:
            print(f"WARN: hull {verb} {name} did not return within 120s")


def asset_dir():
    if os.environ.get("HULL_BOOT_ASSETS"):
        return os.environ["HULL_BOOT_ASSETS"]
    out = hull("assets", "dir", split=True)
    if out.returncode == 0 and out.stdout.strip():
        return out.stdout.strip().splitlines()[-1]
    return os.path.expanduser("~/.hull/assets")


def boot(name, nested, kernel, initrd):
    """Start name detached and wait until the guest agent answers."""
    cleanup(name)  # a leftover from an interrupted run still holds the name
    args = ["run", "--detach", "--hypervisor", "hvi", "--net", "none",
            "--cpus", "2", "--mem", "1024", "--name", name,
            "--annotation", f"com.urunc.unikernel.bootKernel={kernel}",
            "--annotation", f"com.urunc.unikernel.bootInitrd={initrd}"]
    if nested:
        args.append("--nested-virt")
    run = hull(*args, IMAGE, timeout=300)
    if run.returncode != 0:
        die(f"hull run {name} exited {run.returncode}", run.stdout)
    deadline = time.monotonic() + BOOT_TIMEOUT
    last = None
    while time.monotonic() < deadline:
        last = hull("exec", name, "--", "true", timeout=30)
        if last.returncode == 0:
            return
        time.sleep(2)
    die(f"{name}: guest agent did not answer within {BOOT_TIMEOUT}s",
        (last.stdout if last else "") + "\n" + hull("logs", name).stdout)


def guest(name, script):
    out = hull("exec", name, "--", "sh", "-c", script, timeout=60)
    if out.returncode != 0:
        die(f"exec into {name} exited {out.returncode}", out.stdout)
    return out.stdout


def inspect(name):
    out = hull("inspect", name, split=True)
    try:
        return json.loads(out.stdout)
    except ValueError:
        die(f"hull inspect {name} did not print JSON (exit {out.returncode})",
            out.stdout + out.stderr)


def stop_and_check(name):
    """Stop name and assert no hvi is left running for it."""
    stop = hull("stop", name, timeout=120)
    if stop.returncode != 0:
        die(f"hull stop {name} exited {stop.returncode}", stop.stdout)
    # The L2 guests live in the L1's memory, and the L1's hvi going away is
    # what ends them. Look for that process itself: ps only reads the record.
    # pgrep exits 1 for "no match" and 2 or more when it could not look,
    # which must not read as a clean stop. The pattern cannot start with a
    # dash: macOS pgrep takes it as an option and exits 2.
    left = subprocess.run(["pgrep", "-f", f"hvi boot .*sandbox-id {name}( |$)"],
                          stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
    if left.returncode == 0:
        die(f"an hvi for {name} is still running after stop: pid {left.stdout.split()}")
    if left.returncode != 1:
        die(f"pgrep exited {left.returncode}; cannot confirm the hvi for {name} is gone", left.stdout)
    cleanup(name)


def preflight():
    if platform.system() != "Darwin" or platform.machine() != "arm64":
        skip(f"hvi needs Apple Silicon, this is {platform.system()}/{platform.machine()}")
    if not os.path.exists(BIN):
        skip(f"no hull binary at {BIN} (run: make macos)")
    if not (os.access(os.path.join(os.path.dirname(os.path.abspath(BIN)), "hvi"), os.X_OK)
            or shutil.which("hvi")):
        skip("no hvi binary next to hull or on PATH (run: make macos)")

    # Ask hull, which asks the same hvi a run would start.
    caps = hull("capabilities", "--json", split=True)
    try:
        nested_caps = json.loads(caps.stdout)["nestedVirt"]
    except (ValueError, KeyError, TypeError):
        die(f"hull capabilities --json gave no readable answer (exit {caps.returncode})",
            caps.stdout + caps.stderr)
    if nested_caps.get("supported") is not True:
        detail = nested_caps.get("detail", "")
        # `is True`: a document without the field is not an answer.
        if nested_caps.get("answered") is not True:
            die(f"hull capabilities got no answer from hvi: {detail}")
        if REQUIRE:
            die(f"HULL_REQUIRE_NESTED=1 and this host has no nested virtualization: {detail}")
        skip(f"this host has no nested virtualization: {detail}")

    assets = asset_dir()
    kernel, initrd = os.path.join(assets, "Image"), os.path.join(assets, "container-initrd")
    for path in (kernel, initrd):
        if not os.path.exists(path):
            skip(f"no boot artifact at {path}; set HULL_BOOT_ASSETS")
    return kernel, initrd


def main(kernel, initrd):
    # ---- with --nested-virt ----
    boot(NESTED, True, kernel, initrd)
    out = guest(NESTED, "test -c /dev/kvm && echo KVM_DEV_OK; dmesg")
    if "KVM_DEV_OK" not in out:
        die("--nested-virt guest has no /dev/kvm character device", out)
    if "CPU: All CPU(s) started at EL2" not in out:
        die("--nested-virt guest kernel did not start at EL2", out)
    if "initialized successfully" not in out or "kvm [1]:" not in out:
        die("--nested-virt guest kernel did not initialise KVM", out)
    if "started in inconsistent modes" in out:
        die("--nested-virt guest CPUs started at different exception levels", out)
    if "Brought up 1 node, 2 CPUs" not in out:
        die("--nested-virt guest did not bring up both vCPUs", out)
    if inspect(NESTED).get("nestedVirt") is not True:
        die(f"hull inspect {NESTED} does not record nestedVirt: true")
    kvm_lines = [l.strip() for l in out.splitlines() if "kvm [1]:" in l]
    stop_and_check(NESTED)

    # ---- without it: the default must stay off, and say so ----
    boot(CONTROL, False, kernel, initrd)
    out = guest(CONTROL, "test -e /dev/kvm && echo KVM_DEV_PRESENT; dmesg")
    if "KVM_DEV_PRESENT" in out:
        die("a guest booted without --nested-virt has /dev/kvm", out)
    if "started at EL2" in out:
        die("a guest booted without --nested-virt started at EL2", out)
    # Absence alone would also pass a guest whose log was empty or cut short.
    if "CPU: All CPU(s) started at EL1" not in out or "HYP mode not available" not in out:
        die("a guest booted without --nested-virt did not report EL1 and no HYP mode", out)
    record = inspect(CONTROL)
    if "nestedVirt" in record or "--nested-virt" in record.get("cmdLine", []):
        die(f"hull inspect {CONTROL} records nested virtualization it was not asked for")
    stop_and_check(CONTROL)
    return kvm_lines


try:
    kernel, initrd = preflight()
except Failed as e:
    print(f"FAIL: {e}")
    sys.exit(1)
try:
    lines = main(kernel, initrd)
except Failed as e:
    print(f"FAIL: {e}")
    sys.exit(1)
finally:
    for name in (NESTED, CONTROL):
        cleanup(name)
for line in lines:
    print(f"  {line}")
print(f"PASS: {IMAGE} got EL2 and KVM with --nested-virt, and EL1 without it")
