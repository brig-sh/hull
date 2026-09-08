# Test harnesses

Everything in this directory boots a real VM through a built `hull`, except
`compose-config-smoke.sh`, which only runs `compose config`. The Go unit
tests live next to the code and run with `make test`; these scripts are the
end-to-end layer on top of them, and they need an Apple Silicon host with
working HVF. Each Python harness skips with a named reason, exit 0, when the
host cannot run it.

## Environment

| variable | read by | meaning |
|---|---|---|
| `HULL_BIN` | every `.py` harness except `pty-jobcontrol-test.py`, which takes the binary as its first argument | the `hull` binary to drive; default `dist/hull_arm64`, the `make macos` output |
| `HULL_STORE_DIR` | every `.py` harness | passed as `--store-dir` when set, so a run never touches `~/.hull/store`. CI uses a store beside the runner's temp dir; the SIP jobs use `$HOME/hull-ci-store` because a long path overflows a unix socket address |
| `HULL_TEST_LOG_DIR` | `pty-checkpoint-test.py` | when set, the console transcript is written to `<dir>/<name>.log`; CI uploads that directory as an artifact when the matrix fails |

hull itself reads none of these. A detached instance's console lands in the
store, at `<store>/instances/<id>/log` (`~/.hull/store/instances/<id>/log`
by default), which is what `hull logs` prints and what the share tests read
back.

## Files

Harnesses CI runs:

- `pty-terminal-test.py <vz|qemu> <name> <intr|double|term|type>` boots a
  foreground run on a PTY, delivers the scenario (Ctrl-C, a double Ctrl-C,
  SIGTERM, or typing into the guest shell), and asserts the parent exits, no
  VMM is left behind, nothing lands on the tty after exit, and the instance
  reads stopped.
- `pty-jobcontrol-test.py <hull> <vz|qemu> <name>` emulates an interactive
  shell's job control (session leader with a ctty, hull in its own
  foreground process group, `WUNTRACED`) and reports a SIGTTOU-suspended
  hull instead of hanging on it.
- `pty-checkpoint-test.py <name>` boots a block-rootfs vz run, starts a tick
  counter in the guest, checkpoints mid-count, checks the VM keeps counting,
  stops it, restores, and asserts the counter continues from the checkpointed
  value.
- `hvi-boot-test.py <name>` boots an unmodified OCI image on the hvi backend
  and asserts the image's own entrypoint ran and the instance stopped with
  no VMM left. Skips without the `hvi` binary, the boot assets or Apple
  Silicon.
- `share-test.py <vz|qemu> <name> <mode>` boots a detached run with
  `--shared-dir` from the user's home, seeds a script into the share, and
  reads the result back from the instance log. Modes: `readwrite`,
  `ownership`, `persist`, `multi`, `nested`, `negative`, and `swap`, which
  shares by descriptor (`--shared-dir-fd`) and checks the share survives the
  directory being swapped out from under the name it was opened as. Sharing
  a TCC-gated location (`~/Documents`, `~/Desktop`, `~/Downloads`) needs Full
  Disk Access and is out of scope; use a plain path.
- `rosetta-test.py <name>` boots a linux/amd64 rootfs under the arm64 guest
  kernel with `--rosetta`, and proves translation by running the rootfs's own
  x86_64 `/bin/sh` through the guest agent. Skips without Rosetta.
- `compose-config-smoke.sh <hull>` runs `compose config` against fixtures
  and asserts the supported surface renders deterministically, ignored keys
  warn on stderr, and static mistakes fail at load. No VM.

Image builders and scripted QEMU checks, run by hand:

- `Dockerfile.ubuntu-qemu` is an Ubuntu image with a kernel and a 9p
  auto-mounting `/init`, in Bunny syntax; `build-ubuntu-test.sh` builds it
  with the `bunny` CLI and `build-with-docker.sh` with `docker build` and the
  Bunny BuildKit frontend, both to `localhost/ubuntu-qemu:aarch64`.
- `Dockerfile.ubuntu-bootable`, `Dockerfile.ubuntu-simple` and
  `Dockerfile.test-linux` are plainer Ubuntu-plus-kernel images whose `/init`
  mounts the pseudo-filesystems and drops to a shell.
- `prepare-ubuntu-bundle.sh`, `prepare-ubuntu-disk.sh`,
  `prepare-ubuntu-minimal.sh` and `setup-minimal-bundle.sh` build an OCI
  bundle by hand (kernel plus initrd, disk image, minimal initrd, or Alpine)
  for the QEMU backend when Bunny is not available.
- `test-ubuntu-qemu.sh [image] [timeout]` boots the Bunny image on QEMU and
  checks boot, log capture, kernel messages, instance state, 9p sharing and
  cleanup; `test-qemu-bundle.sh` and `test-qemu-ubuntu.sh` do the same
  against the hand-built bundles.
- `test-shim-bundle.sh` exercises a `containerd-shim-urunc-v2` this
  repository no longer builds. Nothing runs it.

The PTY harnesses boot `harbor.nbfc.io/nubificus/urunc-ubuntu-vz:aarch64`.

## How CI runs them

`.github/workflows/ci.yml` gates the VM tiers on a code change (Go, Swift,
the hvi submodule, `test/`, workflows) or a manual dispatch; a docs-only
change runs lint and the unit suite and skips the rest. The VM steps run only
on the self-hosted Apple Silicon runners, so a fork pull request, which lands
on a GitHub-hosted runner, stops at the unit suite.

- `build` runs `make macos`, smoke-tests the binary and runs
  `test/compose-config-smoke.sh ./dist/hull_arm64`.
- `unit + e2e` runs the unit suite, then, signed with the Developer ID, the
  PTY matrix (`pty-terminal-test.py` vz `type`/`intr`/`double`/`term` and
  qemu `type`/`intr`, `pty-jobcontrol-test.py` on vz and qemu,
  `pty-checkpoint-test.py`), `hvi-boot-test.py`, the share matrix
  (`share-test.py` on vz in all seven modes, on qemu in `readwrite`,
  `ownership` and `persist`), and `rosetta-test.py`. It asks for a runner
  with a console session (`gui` label): checkpoint has never passed without
  one.
- `microVM boot (SIP-enabled)` and `microVM boot (SIP-disabled)` each boot
  `pty-terminal-test.py vz ... type` and `hvi-boot-test.py`, the first with a
  Developer ID signature, the second ad-hoc signed, on runners labelled for
  their SIP state.

Run the same thing locally against a fresh build:

```bash
make macos
export HULL_BIN=$PWD/dist/hull_arm64 HULL_STORE_DIR=$HOME/hull-test-store
python3 test/pty-terminal-test.py vz t1 type
python3 test/share-test.py vz s1 readwrite
python3 test/hvi-boot-test.py h1
test/compose-config-smoke.sh "$HULL_BIN"
```
