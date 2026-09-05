# Changelog

All notable changes to hull. Generated from the Conventional-Commits
history; each entry links to the PR that introduced it.
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


