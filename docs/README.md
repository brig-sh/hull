# hull documentation

hull boots an OCI image as a real virtual machine on an Apple Silicon Mac.
The [top-level README](../README.md) is the introduction. This page is the map.

Pages are grouped by what you are trying to do. Read the tutorial first if you
have never run hull.

## Start here

| Page | What it covers |
|---|---|
| [install.md](install.md) | Install a release, verify its signature, put the executables on your PATH |
| [running.md](running.md) | Boot a plain container image, then run, inspect, exec into and remove an instance |

## Task guides

| Page | What it covers |
|---|---|
| [backends.md](backends.md) | The `vz`, `hvi` and `qemu` backends compared, with host requirements and real gaps |
| [storage.md](storage.md) | The store, what survives stop and remove, shared host directories, named volumes |
| [networking.md](networking.md) | Backend-native networking, the user-mode gateway, DNS, port forwards |
| [network-egress.md](network-egress.md) | Restricting what a guest may reach, and what the filter does not cover |
| [compose.md](compose.md) | Running several services together, one VM per service |
| [checkpoint-restore.md](checkpoint-restore.md) | Saving a running VM and resuming it later |
| [images.md](images.md) | Pull policy, digests, platforms, annotations, boot assets, private registries |
| [troubleshooting.md](troubleshooting.md) | Errors you are likely to hit, and what each one means |

## Reference

| Page | What it covers |
|---|---|
| [cli.md](cli.md) | Every command, flag and environment variable |
| [telemetry.md](telemetry.md) | Exactly what telemetry is sent, and how to turn it off |
| [performance.md](performance.md) | Measured numbers, with the hardware, commands and limits behind them |

## Explanations

| Page | What it covers |
|---|---|
| [architecture.md](architecture.md) | How the CLI, the runners, the frameworks and the guest fit together |
| [compose-support.md](compose-support.md) | Historical design record from July 2026. Not a usage guide |

## Contributing

| Page | What it covers |
|---|---|
| [build.md](build.md) | Build hull, `vz-runner` and `hvi` from source, and run the tests |
| [signing.md](signing.md) | Code-signing entitlements, local development signing, notarization |
| [releasing.md](releasing.md) | The release process, for maintainers |
| [../CONTRIBUTING.md](../CONTRIBUTING.md) | Commit, review and merge conventions |
| [../AI_POLICY.md](../AI_POLICY.md) | Policy on AI-assisted contributions |
| [../test/README.md](../test/README.md) | The end-to-end harnesses and how CI runs them |

## Component documentation

hull delegates work to three projects with their own documentation:

- [urunc](https://github.com/urunc-dev/urunc) supplies the darwin hypervisor
  backends and the generic container initrd. hull pins a specific commit; see
  [build.md](build.md#the-urunc-dependency).
- [hvi](https://github.com/brig-sh/hvi-vmm) is the Rust VMM behind the `hvi`
  backend, and a git submodule of this repository.
- [brig](https://github.com/brig-sh/brig) drives hull on macOS. If you came
  here from brig, most of what you want is in [cli.md](cli.md) and
  [storage.md](storage.md).
