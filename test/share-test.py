#!/usr/bin/env python3
# Shared-folder (--shared-dir) smoke tests, focused on sharing a directory
# from the user's home into the guest. Boots a real VM, drives the guest with
# a small script seeded INTO the shared dir (so no in-guest agent or PTY is
# needed), and asserts both directions plus ownership and persistence.
#
# The guest runs `/bin/sh /mnt/share/runtest.sh`; urunc prepends the image's
# /urunit, so the init wrapper still mounts the share before the script runs,
# and the script's stdout lands on the console (the detached instance log).
#
# Usage: URUNC_MACOS_BIN=dist/urunc-macos_arm64 \
#        python3 test/share-test.py <vz|qemu> <instance-name> <mode>
#   modes: readwrite | ownership | persist | multi | nested | negative
import os
import sys
import time
import shutil
import subprocess

BIN = os.environ.get("URUNC_MACOS_BIN", "dist/urunc-macos_arm64")
STORE = os.environ.get("URUNC_STORE_DIR", "")
GLOBAL = (["--store-dir", STORE] if STORE else [])
IMAGE = os.environ.get("URUNC_TEST_IMAGE", "harbor.nbfc.io/nubificus/urunc-ubuntu-vz:aarch64")
BOOT_TIMEOUT = int(os.environ.get("URUNC_TEST_BOOT_TIMEOUT", "180"))

hv = sys.argv[1] if len(sys.argv) > 1 else "vz"
name = sys.argv[2] if len(sys.argv) > 2 else "share-e2e"
mode = sys.argv[3] if len(sys.argv) > 3 else "readwrite"

# Workspaces live under the user's HOME on purpose -- that is the case we care
# about. We use a dotted temp dir, never a TCC-gated one (~/Documents etc.),
# which would need Full Disk Access and is a documented limitation.
HOME = os.path.expanduser("~")
workspaces = []


def cli(args, **kw):
    return subprocess.run([BIN] + GLOBAL + args, capture_output=True, text=True, **kw)


def cleanup():
    cli(["stop", name])
    cli(["rm", name])
    for ws in workspaces:
        shutil.rmtree(ws, ignore_errors=True)


def die(reason):
    print(f"FAIL [{hv}/{mode}]: {reason}")
    log = cli(["logs", name]).stdout
    if log:
        print("---- console tail ----")
        print(log[-2500:])
        print("----------------------")
    cleanup()
    sys.exit(1)


def mkworkspace(suffix=""):
    ws = os.path.join(HOME, f".urunc-ci-share-{name}{suffix}")
    shutil.rmtree(ws, ignore_errors=True)
    os.makedirs(ws)
    workspaces.append(ws)
    return ws


def write(path, content, mode=0o644):
    with open(path, "w") as f:
        f.write(content)
    os.chmod(path, mode)


def seed_runtest(ws, body):
    """Write the guest-side script into the share and keep the VM alive."""
    write(os.path.join(ws, "runtest.sh"),
          "#!/bin/sh\n" + body + '\necho SHARE_TEST_DONE\nsleep 300\n', 0o755)


def boot(shares, guest_script="/mnt/share/runtest.sh"):
    """Boot detached with one or more --shared-dir mounts."""
    cli(["stop", name]); cli(["rm", name])
    args = ["run", "--detach", "--hypervisor", hv, "--net", "none", "--name", name]
    for host, guest in shares:
        args += ["--shared-dir", f"{host}:{guest}"]
    args += [IMAGE, "/bin/sh", guest_script]
    r = cli(args)
    if r.returncode != 0:
        die(f"run failed: {r.stderr.strip() or r.stdout.strip()}")


def wait_marker(marker, timeout=BOOT_TIMEOUT):
    end = time.time() + timeout
    while time.time() < end:
        if marker in cli(["logs", name]).stdout:
            return cli(["logs", name]).stdout
        time.sleep(2)
    die(f"timed out waiting for {marker!r} on the console")


def host_uid_of(path):
    return subprocess.run(["stat", "-f", "%u", path], capture_output=True, text=True).stdout.strip()


# ---- modes -----------------------------------------------------------------

def readwrite():
    ws = mkworkspace()
    write(os.path.join(ws, "seed.txt"), "hello-from-host")
    seed_runtest(ws,
                 'echo "SEED=[$(cat /mnt/share/seed.txt 2>/dev/null)]"\n'
                 'echo "hello-from-guest" > /mnt/share/from-guest.txt '
                 '&& echo GUEST_WRITE=ok || echo GUEST_WRITE=fail')
    boot([(ws, "/mnt/share")])
    log = wait_marker("SHARE_TEST_DONE")
    if "SEED=[hello-from-host]" not in log:
        die("guest did not see the host-seeded file (host->guest read)")
    if "GUEST_WRITE=ok" not in log:
        die("guest could not write into the share")
    out = os.path.join(ws, "from-guest.txt")
    if not os.path.exists(out):
        die("guest-written file did not appear on the host (guest->host write)")
    if open(out).read().strip() != "hello-from-guest":
        die("guest-written file has the wrong content on the host")
    print(f"PASS [{hv}/readwrite]: bidirectional share works")


def ownership():
    ws = mkworkspace()
    write(os.path.join(ws, "seed.txt"), "owned-by-host")
    seed_runtest(ws,
                 'echo "SEED=[$(cat /mnt/share/seed.txt 2>/dev/null)]"\n'
                 'echo guestdata > /mnt/share/owned.txt && echo GUEST_WRITE=ok || echo GUEST_WRITE=fail')
    boot([(ws, "/mnt/share")])
    log = wait_marker("SHARE_TEST_DONE")
    if "SEED=[owned-by-host]" not in log or "GUEST_WRITE=ok" not in log:
        die("guest could not read the host file or write back")
    out = os.path.join(ws, "owned.txt")
    if not os.path.exists(out):
        die("guest-written file missing on host")
    want, got = str(os.getuid()), host_uid_of(out)
    if got != want:
        die(f"guest-written file owned by uid {got}, expected the host user {want} "
            f"(uid mapping broken -- files would be unusable from the Mac)")
    print(f"PASS [{hv}/ownership]: guest writes land owned by the host user ({want})")


def persist():
    ws = mkworkspace()
    seed_runtest(ws, 'echo persisted > /mnt/share/keep.txt && echo GUEST_WRITE=ok')
    boot([(ws, "/mnt/share")])
    wait_marker("SHARE_TEST_DONE")
    if not os.path.exists(os.path.join(ws, "keep.txt")):
        die("first boot did not write the file")
    cli(["stop", name]); cli(["rm", name])
    # second boot, same workspace: the file must still be visible in the guest.
    seed_runtest(ws, 'test -f /mnt/share/keep.txt && echo FOUND=[$(cat /mnt/share/keep.txt)] || echo FOUND=missing')
    boot([(ws, "/mnt/share")])
    log = wait_marker("SHARE_TEST_DONE")
    if "FOUND=[persisted]" not in log:
        die("file written on the first boot was not visible after restart")
    print(f"PASS [{hv}/persist]: share contents survive a restart")


def multi():
    a, b = mkworkspace("-a"), mkworkspace("-b")
    write(os.path.join(a, "a.txt"), "alpha")
    write(os.path.join(b, "b.txt"), "beta")
    seed_dir = a  # runtest.sh lives in the first share
    write(os.path.join(a, "runtest.sh"),
          '#!/bin/sh\n'
          'echo "A=[$(cat /mnt/a/a.txt 2>/dev/null)]"\n'
          'echo "B=[$(cat /mnt/b/b.txt 2>/dev/null)]"\n'
          'echo SHARE_TEST_DONE\nsleep 300\n', )
    os.chmod(os.path.join(a, "runtest.sh"), 0o755)
    boot([(a, "/mnt/a"), (b, "/mnt/b")], guest_script="/mnt/a/runtest.sh")
    log = wait_marker("SHARE_TEST_DONE")
    if "A=[alpha]" not in log or "B=[beta]" not in log:
        die("both --shared-dir mounts not visible in the guest")
    print(f"PASS [{hv}/multi]: multiple shares mount independently")


def nested():
    ws = mkworkspace()
    write(os.path.join(ws, "n.txt"), "nested-ok")
    seed_runtest(ws, 'echo "N=[$(cat /opt/proj/src/n.txt 2>/dev/null)]"')
    boot([(ws, "/opt/proj/src")], guest_script="/opt/proj/src/runtest.sh")
    log = wait_marker("SHARE_TEST_DONE")
    if "N=[nested-ok]" not in log:
        die("nested guest mount path was not created/mounted")
    print(f"PASS [{hv}/nested]: nested guest mount path works")


def negative():
    # Missing host dir -> clean nonzero exit.
    missing = os.path.join(HOME, f".urunc-ci-nope-{name}")
    shutil.rmtree(missing, ignore_errors=True)
    r = cli(["run", "--detach", "--hypervisor", hv, "--net", "none", "--name", name,
             "--shared-dir", f"{missing}:/mnt/share", IMAGE])
    if r.returncode == 0:
        die("run with a missing host share dir unexpectedly succeeded")
    cli(["stop", name]); cli(["rm", name])
    # Relative guest path -> rejected.
    ws = mkworkspace()
    r = cli(["run", "--detach", "--hypervisor", hv, "--net", "none", "--name", name,
             "--shared-dir", f"{ws}:relative/path", IMAGE])
    if r.returncode == 0:
        die("run with a non-absolute guest path unexpectedly succeeded")
    print(f"PASS [{hv}/negative]: bad --shared-dir args are rejected")


MODES = {"readwrite": readwrite, "ownership": ownership, "persist": persist,
         "multi": multi, "nested": nested, "negative": negative}

if mode not in MODES:
    print(f"unknown mode {mode!r}; choose from {', '.join(MODES)}")
    sys.exit(2)

try:
    MODES[mode]()
finally:
    cleanup()
