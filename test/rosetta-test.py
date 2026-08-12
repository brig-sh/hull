#!/usr/bin/env python3
# Rosetta end-to-end test: boot an unmodified linux/amd64 rootfs under the
# arm64 guest kernel with the translator attached, and prove translation by
# running the rootfs's own amd64 binaries through the guest agent.
#
# The invocation deliberately passes --rosetta WITHOUT --platform: the flag
# implies linux/amd64 for the pull, and this test pins that default. The
# proof is two assertions on the same binary: /bin/sh in the guest is an
# x86_64 ELF (od of its header through the agent), and executing it works,
# which without the translator dies with 'Exec format error'.
#
# Requires Rosetta on the host (softwareupdate --install-rosetta); the test
# skips with a named reason when it is absent, exit 0, so non-Rosetta hosts
# and CI runners stay green the way gated conformance cases do.
#
# Usage: HULL_BIN=dist/hull_arm64 \
#        python3 test/rosetta-test.py <instance-name>
import os
import subprocess
import sys
import time

BIN = os.environ.get("HULL_BIN", "dist/hull_arm64")
STORE = os.environ.get("HULL_STORE_DIR", "")
GLOBAL = (["--store-dir", STORE] if STORE else [])
# ubuntu-rosetta is the published amd64-on-rosetta guest (rosetta-proof is
# the local dev artifact and is deliberately not in the publish matrix).
IMAGE = os.environ.get(
    "HULL_ROSETTA_TEST_IMAGE", "ghcr.io/nofireai/urunc-ubuntu-rosetta:amd64")
BOOT_TIMEOUT = int(os.environ.get("HULL_TEST_BOOT_TIMEOUT", "180"))

name = sys.argv[1] if len(sys.argv) > 1 else "rosetta-e2e"


def cli(args, **kw):
    return subprocess.run([BIN] + GLOBAL + args, capture_output=True, text=True, **kw)


def cleanup():
    cli(["stop", name])
    cli(["rm", name])


def die(reason):
    print(f"FAIL [rosetta]: {reason}")
    log = cli(["logs", name]).stdout
    if log:
        print("---- instance log ----")
        print(log[-4000:])
    cleanup()
    sys.exit(1)


# Rosetta present on the host? Same probe Apple documents: a translated
# no-op. Absent Rosetta is a host property, not a regression, so skip.
probe = subprocess.run(["arch", "-x86_64", "/usr/bin/true"],
                       capture_output=True)
if probe.returncode != 0:
    print("SKIP [rosetta]: Rosetta is not installed on this host "
          "(softwareupdate --install-rosetta)")
    sys.exit(0)

cleanup()  # a previous aborted run must not fail this one

# Boot detached, pinning Vz: rosetta is a Virtualization.framework
# facility, and the image's hypervisor annotation must not steer this test
# onto QEMU. /bin/sleep here is the rootfs's amd64 sleep: reaching
# 'running' at all already needs the binfmt handler registered by .vz-init.
r = cli(["run", "--detach", "--name", name, "--hypervisor", "vz",
         "--rosetta", "--", IMAGE, "/bin/sleep", "86400"])
if r.returncode != 0:
    die(f"run --rosetta failed: {r.stderr.strip() or r.stdout.strip()}")

# The agent (arm64, native) comes up a beat after the VMM; poll through it.
deadline = time.time() + BOOT_TIMEOUT
elf = None
while time.time() < deadline:
    r = cli(["exec", name, "--", "/bin/sh", "-c",
             "od -An -tx1 -N20 /bin/sh; uname -m"])
    if r.returncode == 0:
        elf = r.stdout
        break
    time.sleep(2)
if elf is None:
    die(f"agent never answered within {BOOT_TIMEOUT}s: "
        f"{r.stderr.strip() or r.stdout.strip()}")

# Two proofs in one exec. The shell that ran this pipeline is the rootfs's
# amd64 /bin/sh, so a successful exit already proves translation; the ELF
# header pins that the binary really is x86_64 (e_machine 0x3e at offset
# 18), so the test cannot silently pass against an arm64 rootfs.
flat = " ".join(elf.split())
if "7f 45 4c 46" not in flat:
    die(f"/bin/sh is not an ELF? od said: {elf!r}")
if " 3e 00" not in flat:
    die(f"/bin/sh is not x86_64 (e_machine != 0x3e): {elf!r}")
print(f"guest reports: {elf.strip().splitlines()[-1]} "
      f"(uname -m; kernel stays arm64, binaries are x86_64)")

# And a plain amd64 binary end to end, asserting its output.
r = cli(["exec", name, "--", "/bin/echo", "rosetta-ok"])
if r.returncode != 0 or "rosetta-ok" not in r.stdout:
    die(f"amd64 /bin/echo did not run: rc={r.returncode} "
        f"out={r.stdout!r} err={r.stderr!r}")

cleanup()
print("PASS [rosetta]: amd64 rootfs boots and its x86_64 binaries run translated")
