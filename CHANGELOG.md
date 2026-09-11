# Changelog

All notable changes to hull. Generated from the Conventional-Commits
history with git-cliff (`scripts/changelog.sh`); the release workflow does
not regenerate it, so run the script and commit the result when cutting a
release. History before v0.1.0-rc21 predates the import of this tree and
is kept below as it was generated then; v0.1.0-rc19 and rc20 were re-cut
imports of the same tree as rc21, and rc23 was a version bump only.

## [0.1.0-rc28] - 2026-09-11

### Bug Fixes

- Depend on cosign from the generated cask
- Accept a comma decimal separator from ps
- Rearm the sampler after the interval settles, not before
- Let the qemu script's CPU check actually run
- Measure cpu_pct as a rate, not ps' %cpu column
- Refuse to unlink a socket we cannot classify
- Require a checkpoint's named artifacts to exist

### Documentation

- Stop cpu_pct's table row promising a 100 ceiling
- Describe cpu_pct as a rate, and name the hvi backend
- Refresh the brand marks to the pixel wordmark
- Describe the self-test cases as they run now
- Correct what the harnesses and CI actually do
- Repoint the checkpoint citations at the rewritten page
- Correct claims the reference pages made about the code
- Replace the README with a task-oriented documentation set
- Correct three flag help strings that had drifted

### Features

- Promote schema_version to an OTLP attribute

### Tests

- Cover the metrics sample and its failure modes
- Pin the terminal filter policy across Go and Swift
- Drop the timeout dependency from the harness self-test
- Address review findings on the harness and guard tests
- Call hull ps, not hull ps -a
- Give the fuzz targets real invariants and seeds
- Make the boot harnesses fail on real failures
- Stop skipping the stop-intent guard tests

### Miscellaneous

- Validate the release config on every change, rather than first reading it on a tag

## [0.1.0-rc27] - 2026-09-08

### Build

- Bump hvi-vmm to c5305f9: virtio-fs serves FORGET, bounds the host resources a guest can pin and leaves host setuid and setgid bits alone on chmod; the macOS VM ends on a failure instead of hanging; x86 serial moves to vm-superio

### Miscellaneous

- Notarize releases on a runner that reaches Apple
- Sync the hvi-vmm submodule URL before fetching it in CI

## [0.1.0-rc26] - 2026-09-08

### Bug Fixes

- Restore the short flags on run, restore, logs, rm and stop
- Refuse a unix socket path the kernel cannot bind
- Say why a checkpoint failed instead of timing out
- Build hvi from the submodule so its toolchain pin applies
- Serve the DNS records the gateway was started with

### Build

- Pin urunc to urunc-dev/urunc feat/initrd-hvi-backend-v0.8.0

### Documentation

- Say what make macos builds and what CI runs
- Regenerate through rc25 and stop claiming CI does it
- Replace the two legacy READMEs
- Document the gateway flags and the checkpoint knobs
- Say where the state lives and what the prompt prints
- Match the compose reference to the shipped command
- Bring the README up to the hull we ship
- Complete the sentences the import cut
- Add release, CI, coverage, Go and license badges
- Add AI policy
- Say a host rule authorizes an address, not a name
- Say that a policy covers a gateway, so give each sandbox one
- Say that refreshed addresses are not held per guest
- State what egress filtering leaves alone

### Features

- Re-resolve the named hosts in the egress rules on a timer
- Add the --egress-default and --egress-allow/deny flags
- Gate and pin DNS so host globs are enforceable
- Enforce the egress policy in the netstack forwarders
- Add the egress policy rules and the DNS pin table

### Refactor

- Build the netstack in-repo, not via virtualnetwork

### Tests

- Prove egress filtering with real bytes on real sockets

## [0.1.0-rc25] - 2026-08-28

### Miscellaneous

- Correct the copyright holder in file headers
- Bump hvi-vmm to main and follow the xattr rename

## [0.1.0-rc24] - 2026-08-25

### Bug Fixes

- Move reference matching into the store and fix the listing split

### Features

- Print the store's records as JSON with full digests
- Accept a directory handle for a share

## [0.1.0-rc22] - 2026-08-25

### Bug Fixes

- Name the real reason a cached image was rejected
- Stop a pull when its context is cancelled
- Keep an index digest inside its own repository
- Keep matching the reference an image was pulled under
- Resolve a digest reference against the local store
- Record the index digest a pull resolved through

### Documentation

- List every command in the README

### Miscellaneous

- Bump to the last of the descriptor path layer
- Bump to the attribute stages
- Bump to the descriptor path layer
- Bump off the stale-descriptor bug
- Bump to the fd-relative lookup and getattr

### Tests

- Fold the image seeding helpers into one

## [0.1.0-rc21] - 2026-08-19

### Features

- Add hull, a microVM runtime for macOS

## [0.1.0-rc18] - 2026-08-09

### Bug Fixes

- Wait for delivery on exit so detached runs report

## [0.1.0-rc17] - 2026-08-09

### Bug Fixes

- Measure the vz VM's XPC helper, not the launcher

## [0.1.0-rc16] - 2026-08-09

### Features

- Sample detached VMs while a session is attached

## [0.1.0-rc15] - 2026-08-07

### Bug Fixes

- Cap crash stacks below the collector body limit

### Documentation

- Align the consent-version paragraph with the client

### Features

- Crash reporting via queue-and-upload-next-run
- Wire command/start/end/metrics events into the CLI
- Client package, consent flow and telemetry subcommand

## [0.1.0-rc14] - 2026-08-01

### Bug Fixes

- Pin the keychain codesign uses, not the search list

## [0.1.0-rc13] - 2026-08-01

### Bug Fixes

- Refuse to package an ad-hoc signed tarball
- Sign the Homebrew tarball with the Developer ID

## [0.1.0-rc12] - 2026-08-01

### Bug Fixes

- Reject out-of-range and negative mem_limit
- Cap the size of user-supplied text files
- Refuse instance names that escape the store
- Run the rosetta wrapper natively and fix the binfmt mask
- Close the supervision gaps review found
- Record deliberate stops on every path, and prove it
- Keep teardown and deliberate stops ahead of the supervisor
- Pre-warm the one-shot agent session before running the job
- Apply the host TERM as a default, not an override
- Write urunit.conf 0600, it carries the guest env
- Follow resizes on the descriptor the size came from
- Size only from the descriptors handed in, not /dev/tty
- Size the pty from the terminal, not stdout
- Propagate store-dir in compose logs

### Documentation

- Lead the README with installing, not building
- Add contributing guide with the NOFire git guidelines
- Correct stale no-exec-facility claims

### Features

- Wire Rosetta translation for amd64 rootfses
- Allow pulling an explicit platform
- Add a --rosetta translator share
- Honor restart policies in the project supervisor
- Accept a bare --env VAR, inheriting it from the host
- Exec layer: compose exec, exec healthchecks, hooks, top
- Record exit status and run one-shot services as jobs
- Named volumes
- Variable interpolation and env_file
- Add config subcommand and warn on every ignored key

### Miscellaneous

- Add issue and PR templates

### Tests

- Fuzz the compose parsers and the name/volume invariants
- Prove the amd64-under-translation path end to end
- Assert the exit code is reachable, not just recorded
- Drive the real stop path, not just the helper
- Add out-of-scope status and re-base the headline score
- Add compose-spec capability manifest and guards
- Vendor pinned compose-spec JSON schema

## [0.1.0-rc11] - 2026-07-20

### Bug Fixes

- Check the progress writer's error returns
- Make image pulls reliable

### Features

- Report progress while pulling an image

### Performance

- Skip the unpack when the digest is unchanged

### Revert

- Keep containerd's archive.Apply

## [0.1.0-rc10] - 2026-07-19

### Features

- Add --pull to control image resolution

## [0.1.0-rc9] - 2026-07-19

### Bug Fixes

- Heal a store poisoned by an interrupted pull
- Make fetched kernel artifacts host-readable
- Disable Go VCS stamping in guest builds

### Miscellaneous

- Split the guest images into urunc-images

## [0.1.0-rc8] - 2026-07-18

### Bug Fixes

- Re-sign tarball binaries as standalone code

## [0.1.0-rc7] - 2026-07-18

### Bug Fixes

- Mount --shared-dir shares on QEMU

### Tests

- Add shared-folder smoke tests

## [0.1.0-rc6] - 2026-07-18

### Documentation

- Add a usage reference for the compose command
- Document and script the Claude Code guest image build

### Features

- Automate changelog + release notes
- Build =y virtio-fs/gpu arm64 kernel
- Add Claude Desktop guest image
- --gui mode (windowed VZVirtualMachineView)
- --gui flag for windowed instances

### Tests

- Tolerate the job ending before ^C in job-control test ([#34](https://github.com/brig-sh/hull/pull/34))

## [0.1.0-rc5] - 2026-07-17

### Bug Fixes

- Bump every release-anchored tap formula
- Open the tap PR as the org release bot

## [0.1.0-rc4] - 2026-07-17

### Features

- One-shot release workflow

## [0.1.0-rc3] - 2026-07-17

### Bug Fixes

- Apply whiteouts and hard links in the fallback extractor

### Features

- Run sessions as the image-configured or requested user

## [0.1.0-rc2] - 2026-07-17

### Bug Fixes

- Check the deferred Close error in exec
- Fall back to anonymous pull when credentials are unavailable
- Make image pulls atomic
- Don't get suspended by SIGTTOU after the VMM exits
- Raw host tty for the Vz console — kills the double echo
- Terminal-handling pass for foreground runs
- Total timeout budget and pid-identity guard
- Docker-parity review round
- Socket hygiene, fd-passing hardening, service DNS
- Strict --mac and configurable stop grace
- Review round 1 — staging, cleanup, discovery, volumes
- Resolve all golangci-lint findings

### Build

- Consume urunc via the NOFireAI fork instead of a local path

### Documentation

- Document brew trust for the private tap
- Checkpoint/restore guide for the Vz backend
- Log the boot, rootfs and launch-prologue increments
- Darwin upstreaming analysis, convergence progress, and Phase 2 seams
- Add compose stack review notes and macOS landscape comparison
- Review follow-ups
- Mark Phase 3 lifecycle and storage as implemented
- Record the vmnet NAT guest isolation finding
- Add exploration of docker-compose support
- Explain the urunc fork pin and how to refresh it

### Features

- Run commands in a running instance via urunit-agent
- Package a Homebrew tarball and publish releases on tags
- Add checkpoint and restore commands
- Checkpoint and restore VM state
- Adopt the converged urunc fork
- Drag-to-Applications DMG with branded icons
- QEMU services are full citizens
- QEMU gateway networking and tagged shares on both backends
- Accept QEMU stream members
- TCP healthchecks, gated depends_on, all volumes
- Repeatable --shared-dir mounted at the guest path
- Force stop when the guest ignores requestStop
- Try SIGTERM before SIGKILL
- Gateway-backed networking with static IPs and ports
- Join the gateway with --gateway-sock/--gateway-ip
- Back the NIC with an inherited fd via --net-fd
- User-mode network gateway on gvisor netstack
- Up/down/ps/logs for a compose-file subset
- Guest IP discovery, env/command overrides, --add-host
- Instance MAC/IP fields, duplicate-name check, store lock
- Accept --mac for a deterministic NAT device address
- Import hull CLI, vz-runner, docs, and tests

### Miscellaneous

- Bump urunc pin to darwin/converge rebased on latest upstream main
- Pin urunc to cleaned-up darwin/converge branch
- Refresh go.sum for urunc dependency bumps

### Tests

- Checkpoint/restore PTY harness
- Marker-based boot waiting and failure transcripts in the harness


