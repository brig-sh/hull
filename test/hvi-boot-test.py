#!/usr/bin/env python3
# HVI generic-container boot harness: boots an unmodified OCI image on the hvi
# backend and asserts the image's own entrypoint ran, then that the instance
# stopped and left no VMM behind.
#
# This is the one test that exercises the whole hvi chain end to end -- the
# signed VMM, the host kernel and initrd, the APFS rootfs clone and the
# virtio-fs export -- so it needs a real machine and cannot run in a hosted
# CI container.
#
# It SKIPS (exit 0) rather than fails when the machine cannot run it: no hvi
# binary, no boot artifacts, or not Apple Silicon. A skip is reported on
# stdout so a CI log says which prerequisite was missing. Every other failure
# is a real one.
#
# Usage: HULL_BIN=dist/hull_arm64 python3 test/hvi-boot-test.py <name>
import os, platform, shutil, subprocess, sys

BIN = os.environ.get("HULL_BIN", "dist/hull_arm64")
STORE = os.environ.get("HULL_STORE_DIR", "")
GLOBAL_ARGS = ["--store-dir", STORE] if STORE else []
# Ask hull where the boot assets are rather than assuming. They live under the
# store, so the answer depends on --store-dir, and a test that hardcoded a path
# would either skip (looking in an empty directory) or -- worse -- boot a kernel
# belonging to some other store.
def asset_dir():
    if os.environ.get("HULL_BOOT_ASSETS"):
        return os.environ["HULL_BOOT_ASSETS"]
    try:
        out = subprocess.run([BIN, *GLOBAL_ARGS, "assets", "dir"],
                             capture_output=True, text=True, timeout=60)
        if out.returncode == 0 and out.stdout.strip():
            return out.stdout.strip()
    except (OSError, subprocess.SubprocessError):
        pass
    # An older hull without `assets dir`: fall back to where it used to put them.
    return os.path.expanduser("~/.hull/assets")


ASSETS = asset_dir()
IMAGE = os.environ.get("HULL_HVI_IMAGE", "docker.io/library/ubuntu:latest")
name = sys.argv[1] if len(sys.argv) > 1 else "hvi-boot"
TOKEN = f"hvi-booted-{name}"


def skip(reason):
    print(f"SKIP: {reason}")
    sys.exit(0)


def die(reason, output=b""):
    print(f"FAIL: {reason}")
    if output:
        print("---- console transcript ----")
        sys.stdout.buffer.write(output[-4000:])
        sys.stdout.flush()
    cleanup()
    sys.exit(1)


def cleanup():
    for verb in ("stop", "rm"):
        subprocess.run([BIN] + GLOBAL_ARGS + [verb, name],
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


if platform.system() != "Darwin" or platform.machine() != "arm64":
    skip(f"hvi needs Apple Silicon, this is {platform.system()}/{platform.machine()}")
if not os.path.exists(BIN):
    skip(f"no hull binary at {BIN} (run: make macos)")

# hvi is discovered next to the hull executable, then on PATH -- the same
# order the runtime uses, so this looks where a real run would look.
if not (os.access(os.path.join(os.path.dirname(os.path.abspath(BIN)), "hvi"), os.X_OK)
        or shutil.which("hvi")):
    skip("no hvi binary next to hull or on PATH (run: make macos)")

kernel = os.path.join(ASSETS, "Image")
initrd = os.path.join(ASSETS, "container-initrd")
for path in (kernel, initrd):
    if not os.path.exists(path):
        skip(f"no boot artifact at {path}; set HULL_BOOT_ASSETS")

cleanup()  # a leftover instance from an interrupted run still holds the name

# --net none deliberately: this asserts that a stock image boots and runs its
# entrypoint, not that it can reach the network. Networking on hvi needs the
# gateway and is a separate concern.
run = subprocess.run(
    [BIN] + GLOBAL_ARGS + [
        "run", "--hypervisor", "hvi", "--rootfs-type", "virtiofs",
        "--net", "none", "--name", name,
        "--annotation", f"com.urunc.unikernel.bootKernel={kernel}",
        "--annotation", f"com.urunc.unikernel.bootInitrd={initrd}",
        "--", IMAGE, "/bin/echo", TOKEN,
    ],
    stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=300)
output = run.stdout or b""

if TOKEN.encode() not in output:
    die(f"the image's entrypoint did not run: {TOKEN!r} never reached the console", output)

# A guest that ran is only half of it: the VMM must be gone and the instance
# must read stopped, or the next run inherits a machine nobody is watching.
ps = subprocess.run([BIN] + GLOBAL_ARGS + ["ps", "-a"],
                    stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=60)
for line in (ps.stdout or b"").decode(errors="replace").splitlines():
    fields = line.split()
    if len(fields) >= 2 and fields[0] == name and fields[1] == "running":
        die(f"instance {name} is still running after its command exited", output)

cleanup()
print(f"PASS: {IMAGE} booted unmodified on hvi and ran its entrypoint")
