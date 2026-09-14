#!/usr/bin/env python3
# Unikraft-through-the-gateway harness: runs a Unikraft OCI image with `hull
# run`, on a static address the runtime chooses, and fetches a page from it
# through a host port forward.
#
# What this covers that nothing else does: the address. hull renders
# netdev.ip= and records the address it asked for, so a guest that ignores it
# and leases a different one still looks healthy from the host -- `hull ps`
# reports an address, the instance is running, and only a request through the
# forward says otherwise. A Unikraft image built without
# CONFIG_LIBUKNETDEV_EINFO_LIBPARAM fails exactly that way, so the fetch is
# the assertion and the address check is the diagnosis.
#
# It SKIPS (exit 0) when the machine cannot run it: no hull binary, not Apple
# Silicon, or the image cannot be pulled. Every other failure is a real one.
#
# Usage: HULL_BIN=dist/hull_arm64 python3 test/unikraft-gateway-test.py
import json, os, platform, shutil, socket, subprocess, sys, tempfile, time
import urllib.error, urllib.request

BIN = os.environ.get("HULL_BIN", "dist/hull_arm64")
STORE = os.environ.get("HULL_STORE_DIR", "")
GLOBAL_ARGS = ["--store-dir", STORE] if STORE else []
# Republished 2026-09-13 with CONFIG_LIBUKNETDEV_EINFO_LIBPARAM; the digest is
# the same one hvi-vmm's Unikraft CI job pins. A tag would let a republish
# change what this proves without a commit.
IMAGE = os.environ.get(
    "HULL_UNIKRAFT_IMAGE",
    "ghcr.io/brig-sh/unikraft-httpreply@sha256:"
    "c808d8a34676c0598cfa58b4b65779ea56e6e0b324bbd39048a8c2cddff0cfa3")
GUEST_IP = "10.87.0.10"
GUEST_PORT = 8123
BUDGET = int(os.environ.get("HULL_TEST_BOOT_TIMEOUT", "180"))

workdir = None
gateway = None
instance = None


def skip(reason):
    print(f"SKIP: {reason}")
    cleanup()
    sys.exit(0)


def die(reason, output=""):
    print(f"FAIL: {reason}")
    if output:
        print("---- transcript ----")
        print(output[-4000:])
    cleanup()
    sys.exit(1)


def cleanup():
    if instance:
        subprocess.run([BIN, *GLOBAL_ARGS, "rm", "-f", instance],
                       capture_output=True, timeout=120)
    if gateway and gateway.poll() is None:
        gateway.terminate()
        try:
            gateway.wait(timeout=15)
        except subprocess.TimeoutExpired:
            gateway.kill()
    if workdir:
        shutil.rmtree(workdir, ignore_errors=True)


def free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


if platform.system() != "Darwin" or platform.machine() != "arm64":
    skip("needs Darwin/arm64: the hvi backend runs nowhere else")
if not (os.path.isfile(BIN) and os.access(BIN, os.X_OK)):
    skip(f"no hull binary at {BIN}")

# A unix socket path is capped at 104 bytes, and the QEMU socket is the
# longest name derived from this one, so keep the directory short.
workdir = tempfile.mkdtemp(prefix="/tmp/hull-uk-")
sock = os.path.join(workdir, "uk.gateway.sock")
api = os.path.join(workdir, "uk.api.sock")
port = free_port()

pull = subprocess.run([BIN, *GLOBAL_ARGS, "pull", IMAGE],
                      capture_output=True, text=True, timeout=600)
if pull.returncode != 0:
    skip(f"cannot pull {IMAGE}: {pull.stderr.strip()[:300]}")

gateway = subprocess.Popen(
    [BIN, *GLOBAL_ARGS, "network-gateway", "--socket", sock, "--api", api,
     "--qemu-socket", sock + ".qemu",
     "--forward", f"127.0.0.1:{port}={GUEST_IP}:{GUEST_PORT}"],
    stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
for _ in range(100):
    if os.path.exists(sock):
        break
    if gateway.poll() is not None:
        die("the gateway exited before it created its socket", gateway.stdout.read())
    time.sleep(0.1)
else:
    die("the gateway never created its control socket")

run = subprocess.run(
    [BIN, *GLOBAL_ARGS, "run", "--detach", "--hypervisor", "hvi",
     "--net", "shared", "--gateway-sock", sock,
     "--gateway-cidr", f"{GUEST_IP}/24", IMAGE],
    capture_output=True, text=True, timeout=300)
if run.returncode != 0:
    die(f"hull run failed: {run.stderr.strip()[:600]}", run.stdout)
instance = run.stdout.strip().splitlines()[-1].strip() if run.stdout.strip() else ""
if not instance:
    die("hull run printed no instance id", run.stderr)

# The fetch is the assertion: it only succeeds if the guest is answering on
# the address hull told it to take.
body, last = None, ""
deadline = time.time() + BUDGET
while time.time() < deadline:
    try:
        with urllib.request.urlopen(f"http://127.0.0.1:{port}/", timeout=5) as r:
            if r.status == 200:
                body = r.read().decode("utf-8", "replace")
                break
            last = f"HTTP {r.status}"
    except (urllib.error.URLError, OSError, ConnectionError) as e:
        last = str(e)
    time.sleep(2)

if body is None:
    # Say which of the two failures this is, because they are fixed in
    # different repositories.
    inspect = subprocess.run([BIN, *GLOBAL_ARGS, "inspect", instance],
                             capture_output=True, text=True, timeout=60)
    die(f"no page through the forward after {BUDGET}s (last: {last}); "
        f"if hull's run output warned that the guest took a different address, "
        f"the image ignores netdev.ip and needs CONFIG_LIBUKNETDEV_EINFO_LIBPARAM",
        inspect.stdout + (gateway.stdout.read() if gateway.poll() is not None else ""))

# The host's record has to name the address the guest actually answered on,
# not merely some address.
inspect = subprocess.run([BIN, *GLOBAL_ARGS, "inspect", instance],
                         capture_output=True, text=True, timeout=60)
if inspect.returncode == 0:
    try:
        recorded = json.loads(inspect.stdout)
        if isinstance(recorded, list):
            recorded = recorded[0] if recorded else {}
        ip = recorded.get("IP") or recorded.get("ip") or ""
        if ip and ip != GUEST_IP:
            die(f"hull recorded {ip} but the guest answered on {GUEST_IP}")
    except (ValueError, KeyError, IndexError):
        pass

print(f"PASS: {IMAGE.split('@')[0]} answered on {GUEST_IP}:{GUEST_PORT} "
      f"through 127.0.0.1:{port} ({len(body)} bytes)")
cleanup()
sys.exit(0)
