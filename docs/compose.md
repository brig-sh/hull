# Compose on hull

`hull compose` runs a multi-service Compose file where **each
service is its own lightweight VM** (not a container), wired together on a
private virtual network. It targets the common shape of a dev stack:
a few services that talk to each other by name and expose a port or two to
the host. It is not the full Compose specification.

For the design rationale and phasing, see
[`compose-support.md`](./compose-support.md); this page is the usage
reference for what ships today.

## Commands

```
hull compose [-f FILE] [-p NAME] [--env-file FILE]... [--profile NAME]... COMMAND

hull compose up [-d] [--subnet CIDR] [SERVICE...]
hull compose down [-v|--volumes]
hull compose ps
hull compose logs [-f] [SERVICE]
hull compose config [SERVICE...]
hull compose exec [-T|--no-tty] [-u|--user U] [-e|--env K=V]... [-w|--workdir DIR] SERVICE COMMAND [ARGS...]
hull compose top
```

- `-f, --file`: Compose file (env `COMPOSE_FILE`; default:
 `docker-compose.yml`, `docker-compose.yaml`, `compose.yml`, or
 `compose.yaml` in the working directory).
- `-p, --project-name`: project name (env `COMPOSE_PROJECT_NAME`;
 default: the current directory name). Namespaces the instances, the
 network, and state.
- `--env-file` -- interpolation env file; replaces the `.env` beside the
 compose file. Repeatable, later files win.
- `--profile` -- activate a profile (env `COMPOSE_PROFILES`, comma-separated).
 Repeatable; `*` activates every profile. See [Profiles](#profiles).
- `up` always runs detached (`-d` is accepted for compatibility).
 `--subnet` sets the project's virtual network (default `10.87.0.0/24`).
 Naming services starts those and their dependencies only. Ctrl-C during
 `up` tears down whatever was created.
- `down` stops and removes every service and the gateway; `--volumes` also
 removes the project's named volumes.
- `logs -f` follows a single named service.
- `config` validates the file and prints the effective configuration, in the
 canonical form `docker compose config` prints.
- `exec` runs a command in a running service through the guest agent;
 `top` lists guest processes per service (the agent and `/bin/ps` are
 required for both).

Instances are ordinary hull instances underneath, so
`hull ps` / `logs` / `stop` also see them; `compose ps` filters to
the project.

## Networking model

`up` starts one **user-mode network gateway** for the project and gives
each service a static IP on the subnet (gateway at `.1`, services from
`.10`). The gateway provides:

- **service-to-service switching**: services reach each other directly on
 the subnet;
- **name resolution**: every service name is both a DNS A record on the
 gateway *and* an `/etc/hosts` entry in every guest, so `http://db:5432`
 works whether the guest resolves via DNS or hosts;
- **NAT egress** to the outside world;
- **host port forwards** from `ports:`.

This is the same user-mode gateway path described in
[`networking.md`](./networking.md): unix-socket networking, **no vmnet, no
`com.apple.vm.networking` entitlement, no root, no re-signed QEMU**. For
restricting what these guests may reach, see
[`network-egress.md`](./network-egress.md). Note that a policy belongs to the
gateway, so every service in a project answers to the same rules.

## Supported Compose keys

This section sketches the common shape of what is supported. Anything not
listed here is either unsupported or untested; `hull compose config` is the
quickest way to find out what a given file resolves to.

Note what `compose config` does and does not establish. It loads the file,
resolves interpolation and `extends`, applies profile selection, and warns on
stderr about every key hull ignores. So a clean run proves the file parses and
that every key in it is one hull honors. **It does not prove the services
run**, and it does not exercise the backend, the network or the images. Treat
it as a lint, not as a dry run.

The everyday service keys are `image` (**required**; `build:` is not
supported), `command`, `environment`, `env_file`, `depends_on`, `restart`,
`volumes`, `ports`, `mem_limit`, `cpus`, `profiles`, and `container_name`. Variable interpolation
works the way docker's does: `${VAR}`, defaults (`${VAR:-x}`), required
markers (`${VAR:?msg}`) and the `$$` escape resolve from your environment
first and the `.env` file next to the compose file second (`--env-file`
replaces it). `env_file:` feeds the guest environment, with `environment:`
winning on collisions. Three urunc-specific extensions,
`x-` prefixed so the file stays Compose-valid, tune the VM:

- `x-hypervisor` -- the backend for that service. **Defaults to `vz` when
 unset** (the backend is forced to `vz`, not read from the image annotation).
 Six values are accepted: `vz`, `qemu`, `hvi`, and the aliases `qemu-hvf`
 (folding to `qemu`) and `virtualization` and `apple` (both folding to `vz`).
 The error message for a rejected value lists only the first three.
- `x-healthcheck-tcp` -- `{ port: N, interval: …, retries: N, start_period: … }`,
 a TCP-connect healthcheck the gateway probes, used by
 `depends_on: { condition: service_healthy }`. Defaults: a 1 s interval and
 60 retries. There is no `timeout` key: a connect either succeeds or it
 does not, so a `timeout` in the file is ignored with a warning. Size the
 wait with `retries` and `start_period`.
- `x-oneshot: true`: run the service as a **job**: its `command` runs to
 completion and its exit code is what dependents wait on (see below).

Four standard Compose keys are also supported and were missing from earlier
versions of this list:

- `healthcheck`: the standard Compose healthcheck, run as an exec probe in
 the guest through the agent. `depends_on: { condition: service_healthy }`
 waits on it. The keys read are `test`, `interval`, `timeout`, `retries`,
 `start_period` and `disable`; anything else under `healthcheck:` warns. Only
 the `CMD`, `CMD-SHELL` and `NONE` test forms are accepted. It is **mutually
 exclusive with `x-healthcheck-tcp`**: setting both is an error, not a
 precedence rule. Use `healthcheck` when the probe is a command in the guest,
 and `x-healthcheck-tcp` when a TCP connect is enough.
- `post_start`: hooks that run in the fresh guest after it starts. A failed
 hook fails the `up`. Each entry reads `command` (required), `user` and
 `environment`.
- `pre_stop`: hooks that run before the service is stopped, same three keys.
 These need the service definitions, so `down` reloads the compose file to
 find them; if it cannot reload the file, the hooks are **skipped with a
 warning** and the rest of the teardown still runs.
- `extends`: resolved by compose-go while the file loads, so the service it
 names is merged in before hull walks the document. It is not ignored, and it
 does not warn.

`depends_on` reads `condition` and `required`.

Anything not listed is ignored, and warned about rather than dropped
silently. Two different things happen depending on the key, which is worth
knowing when you are debugging a file:

- A key the Compose **specification defines** but hull does not act on
  produces exactly one warning line on stderr and the load continues:
  `warning: compose: ignoring unsupported key "<dotted.path>": <hint>`.
- A key the specification **does not define**, such as the typo `imagee`,
  **fails the load** at the schema pass. It does not warn.

So a mistyped key name is an error, not a silent no-op. The top-level
`include:` key merges other compose files
into the project (see below). Named volumes work: declare them under the
top-level `volumes:` key and they become store-managed directories
(`<store>/volumes/<project>_<name>`) that persist across `down`/`up` and
are removed only by `down --volumes`. Notable gaps: no `build:`, no
`networks:` customization beyond `--subnet`. The keys named in this section
are the supported set; [`compose-support.md`](./compose-support.md) records
what is deferred and why.

Not everything unsupported is backlog. Orchestration (`deploy`), cross-VM
namespace sharing (`ipc:`/`pid:`/`network_mode: host`), Windows-only keys,
Docker-platform machinery and device passthrough are out of scope for good:
one service is one VM, and those keys assume a shared kernel or a Docker
daemon. Out-of-scope keys still warn just as loudly.

## Profiles

Profiles select the services that start. A service that declares no
`profiles` is a core service and always starts. A service that declares one
or more profiles starts only when one of its own profiles is active.

```yaml
services:
 web:
 image: harbor.nbfc.io/nubificus/urunc/app:latest
 debugger:
 image: harbor.nbfc.io/nubificus/urunc/shell:latest
 profiles:
 - debug
```

Activate a profile with the repeatable `--profile` flag on the `compose`
command, or with `COMPOSE_PROFILES` as a comma-separated list. The value `*`
activates every profile the file declares.

```console
$ hull compose --profile debug up
$ COMPOSE_PROFILES=debug,tools hull compose up
```

`compose config` applies the same selection, so it prints exactly the
services `compose up` starts.

Validation of a disabled service is only partial, and an earlier version of
this page overstated it. The raw schema pass covers the whole document, so a
malformed disabled service still fails the load. But hull's own
urunc-specific checks and compose-go's consistency check both walk only the
**profile-enabled** services. So an invalid `x-hypervisor`, a bad
`container_name` or a broken `depends_on` inside a service no active profile
enables is not caught until you activate it.

Two conditions stop the command:

- An enabled service depends on a service that no active profile enables.
 The error names the profiles that enable the dependency. Docker refuses
 the same case. To start without the dependency, set `required: false` on
 the `depends_on` entry. The runtime then warns, drops the dependency from
 the graph, and starts the dependent.
- No service is enabled at all, because every service in the file declares a
 profile and none of those profiles is active.

```yaml
services:
 web:
 image: harbor.nbfc.io/nubificus/urunc/app:latest
 depends_on:
 seeder:
 condition: service_completed_successfully
 required: false
 seeder:
 image: harbor.nbfc.io/nubificus/urunc/seed:latest
 profiles:
 - tools
```

Profile names are not validated. The compose specification gives a shape in
its prose, but neither the JSON schema nor `compose-go` enforces it, so a
name docker accepts also works here.

An active profile that no service declares is not an error. It produces a
warning, because nothing extra starts and that result looks the same as
success. Docker stays silent in this case, so the warning is an addition.
Which services start is the same either way.

### Naming services instead of profiles

`compose up` and `compose config` take service names. A named service starts
whatever its profiles say, together with the services it declares in
`depends_on`, and nothing else.

```console
$ hull compose up db-migrations
```

This starts `db-migrations` and the `db` it depends on. It does not activate
the `tools` profile, so another service that only shares that profile stays
down. To start every service in a profile, name the profile with
`--profile tools`.

A named service whose dependency sits behind a different profile is still an
error. Activate that profile, or mark the dependency `required: false`.

The whole enabled set is validated before the file is narrowed to the named
services. A second service that no active profile can satisfy therefore fails
`compose up web`, exactly as it fails a bare `compose up`. Docker reports the
same file the same way.

## Split a file with `include`

The top-level `include:` key merges other compose files into the project. The
short form is a path. The long form is a mapping with `path`,
`project_directory`, and `env_file`. An included file can include more
files, with no depth limit; a cycle is reported as a cycle.

```yaml
include:
 - infra/compose.yaml
 - path: shared/compose.yaml
 project_directory: shared
 env_file: shared/release.env
services:
 web:
 image: web:1
```

Relative paths inside an included file resolve against the directory of that
file. If the include sets `project_directory`, the paths resolve against that
directory instead. The rule covers `env_file` entries and bind mounts, so
`config` prints the host path that `up` mounts.

Each included file gets its own interpolation environment: the `env_file` of
the include, or, when the include names none, the `.env` file in the directory
that file's relative paths resolve against. That is the included file's own
directory unless the include set `project_directory`, in which case it is that
directory. An included file does not inherit the environment of the file that
included it.

Two cases were previously documented here as failing the load. Neither does,
at the pinned compose-go version:

- A service defined in **both** an included file and the including file is
  **merged**, not rejected. Nothing in hull adds a rejection. If you did not
  intend a merge, rename one of the two services.
- An include entry that lists **more than one path** is accepted. compose-go
  treats the first as the base and the rest as overrides, and hull's own
  include handling reads a plural list of paths. So the docker merge semantics
  do apply.

Run `hull compose config` to see what either case actually resolved to.

## One-shot services, exit status, and restart policies

`depends_on: { condition: service_completed_successfully }` works, and so
does the `x-oneshot: true` marker that names a job directly. A service
targeted by that condition is a job whether or not it says so. A job boots,
runs its `command` through the guest agent, records the **exact exit code**,
and stops; code 0 releases its dependents, a non-zero code fails `up` with
the code and the job's output and starts nothing that waited on it. Two
consequences worth knowing before you write one:

- A job **requires an agent-bearing image** (one shipping `/urunit-agent`).
 There is no other channel that carries a guest process's exit status
 today, so `up` fails loudly naming the requirement rather than assuming
 success.
- A job's VM runs a benign init and the command runs through the agent, so
 the job's output is **not** in `compose logs`. It is captured and printed
 when the job fails.

```yaml
services:
 migrate:
 image: harbor.nbfc.io/nubificus/my-migrator:aarch64 # ships /urunit-agent
 x-oneshot: true
 command: ["/usr/local/bin/migrate", "up"]
 api:
 image: harbor.nbfc.io/nubificus/my-api:aarch64
 restart: always
 depends_on:
 migrate:
 condition: service_completed_successfully
```

`restart: no | always | on-failure[:N] | unless-stopped` is honored by a
supervision loop inside the per-project gateway daemon, which polls service
liveness and re-runs what disappeared with capped exponential backoff. An
unknown value is a load error. Divergences: a restart is noticed within the
poll interval rather than instantly; `on-failure` **degrades to `always`**
with a load-time warning (a plain service reports no exit code, so a clean
exit cannot be told from a failure; the `:N` attempt cap is still honored);
`always` and `unless-stopped` are indistinguishable here; and a job is never
restarted whatever its policy says. An explicit `hull stop` outranks
every policy: a service you stop **stays stopped**.

Only a job records an exit code. A plain service whose VM ended on its own
reports `-` in `hull ps`'s EXIT column, an honest "ended without a
reportable status", and exactly the gap Phase B closes. `compose
ps` shows the state word only; a recorded code is visible through
`hull ps` and `hull inspect`.

## A complete example you can run

This uses only `ubuntu:latest`, so it needs no private registry and no
packaged image. On the `vz` backend hull supplies the generic kernel and
initrd, so a plain container image boots.

Save this as `compose.yaml` in an empty directory:

```yaml
services:
  greeter:
    image: ubuntu:latest
    x-hypervisor: vz
    mem_limit: 512m
    command: ["/bin/sh", "-c", "echo greeter listening; sleep 3600"]
    volumes:
      - shared-data:/data

  worker:
    image: ubuntu:latest
    x-hypervisor: vz
    mem_limit: 512m
    x-oneshot: true
    depends_on:
      greeter:
        condition: service_started
    command: ["/bin/sh", "-c", "getent hosts greeter && echo worker resolved greeter"]

volumes:
  shared-data:
```

Check what it resolves to before booting anything. This step needs no VM:

```bash
hull compose config
```

That prints the canonical document and warns on stderr about any key hull
ignores. The file above produces no warnings.

Then run it:

```bash
hull compose up
hull compose ps
hull compose logs greeter
```

`worker` is a one-shot job, so `up` waits for it to finish and its exit code
is what dependents would wait on. It resolves `greeter` by name through the
gateway's resolver, which `hull compose up` starts for the project.

Illustrative output from `hull compose logs worker`:

```
10.87.0.3       greeter
worker resolved greeter
```

Clean up:

```bash
hull compose down              # stops both VMs and the gateway; keeps the volume
hull compose down --volumes    # also deletes shared-data
```

CAUTION: `down --volumes` deletes a volume declared `external: true` as
readily as any other. See
[storage.md](storage.md#compose-volumes).

Two things this example does not show, because they need a listening process
in the guest: `ports:` forwarding a host port to a service, and
`x-healthcheck-tcp` probing one. The sections above cover both.

### Verification note

The compose file above was validated with `hull compose config` against a
binary built from this checkout: exit status 0, no ignored-key warnings, and
the named volume resolved to `<store>/volumes/demo_shared-data`. It was not
booted as part of writing this page, so the log output above is marked
illustrative. A successful `compose config` proves the file loads and every
key in it is one hull honors. It does not prove the workload runs.

## Limits

- One service = one VM: a stack of N services boots N VMs (the subnet caps
 the count; a `/24` gives ~244 usable service addresses).
- `--subnet` must be large enough for the services (`up` errors if not).
- `build:` is unsupported: pre-build and push images (see
 [`NOFireAI/urunc-images`](https://github.com/NOFireAI/urunc-images) for the pattern).
