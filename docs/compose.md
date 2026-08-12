# Compose on urunc-macos

`urunc-macos compose` runs a multi-service Compose file where **each
service is its own lightweight VM** (not a container), wired together on a
private virtual network. It targets the common shape of a dev stack —
a few services that talk to each other by name and expose a port or two to
the host — not the full Compose specification.

For the design rationale and phasing, see
[`compose-support.md`](./compose-support.md); this page is the usage
reference for what ships today.

## Commands

```
urunc-macos compose [-f FILE] [-p NAME] up   [--subnet CIDR]
urunc-macos compose [-f FILE] [-p NAME] down
urunc-macos compose [-f FILE] [-p NAME] ps
urunc-macos compose [-f FILE] [-p NAME] logs [-f] [SERVICE]
```

- `-f, --file` — Compose file (env `COMPOSE_FILE`; default:
  `docker-compose.yml`, `docker-compose.yaml`, `compose.yml`, or
  `compose.yaml` in the working directory).
- `-p, --project-name` — project name (env `COMPOSE_PROJECT_NAME`;
  default: the current directory name). Namespaces the instances, the
  network, and state.
- `up` always runs detached (`-d` is accepted for compatibility).
  `--subnet` sets the project's virtual network (default `10.87.0.0/24`).
  Ctrl-C during `up` tears down whatever was created.
- `logs -f` follows a single named service.

Instances are ordinary urunc-macos instances underneath, so
`urunc-macos ps` / `logs` / `stop` also see them; `compose ps` filters to
the project.

## Networking model

`up` starts one **user-mode network gateway** for the project and gives
each service a static IP on the subnet (gateway at `.1`, services from
`.10`). The gateway provides:

- **service-to-service switching** — services reach each other directly on
  the subnet;
- **name resolution** — every service name is both a DNS A record on the
  gateway *and* an `/etc/hosts` entry in every guest, so `http://db:5432`
  works whether the guest resolves via DNS or hosts;
- **NAT egress** to the outside world;
- **host port forwards** from `ports:`.

This is the same user-mode gateway path described in the README's QEMU
section: unix-socket networking, **no vmnet, no
`com.apple.vm.networking` entitlement, no root, no re-signed QEMU**.

## Supported Compose keys

The full, per-capability support matrix — every top-level element, service
key, urunc extension, and compose CLI verb, with its status, divergence
notes, and security tradeoffs — lives in
[`compose-conformance.md`](./compose-conformance.md). That page is
**generated** from the conformance manifest
(`test/conformance/capabilities.yaml`) and is CI-gated against drift, so it
is the authoritative list of what ships today. This section only sketches the
common shape.

The everyday service keys are `image` (**required** — `build:` is not
supported), `command`, `environment`, `env_file`, `depends_on`, `restart`,
`volumes`, `ports`, `mem_limit`, `cpus`, `profiles`, and `container_name`. Variable interpolation
works the way docker's does: `${VAR}`, defaults (`${VAR:-x}`), required
markers (`${VAR:?msg}`) and the `$$` escape resolve from your environment
first and the `.env` file next to the compose file second (`--env-file`
replaces it). `env_file:` feeds the guest environment, with `environment:`
winning on collisions. Three urunc-specific extensions,
`x-` prefixed so the file stays Compose-valid, tune the VM:

- `x-hypervisor` — `vz` or `qemu` per service. **Defaults to `vz` when
  unset** (the backend is forced to `vz`, not read from the image
  annotation).
- `x-healthcheck-tcp` — `{ port: N, interval: …, timeout: … }`, a
  TCP-connect healthcheck the gateway probes, used by
  `depends_on: { condition: service_healthy }`.
- `x-oneshot: true` — run the service as a **job**: its `command` runs to
  completion and its exit code is what dependents wait on (see below).

Anything not listed is ignored (unsupported keys are warned about, not
dropped silently). The top-level `include:` key merges other compose files
into the project (see below). Named volumes work: declare them under the
top-level `volumes:` key and they become store-managed directories
(`<store>/volumes/<project>_<name>`) that persist across `down`/`up` and
are removed only by `down --volumes`. Notable gaps: no `build:`, no
`networks:` customization beyond `--subnet`. See [`compose-conformance.md`](./compose-conformance.md)
for the exhaustive matrix and `compose-support.md` for what's deferred.

Not everything unsupported is backlog. 21 capabilities are declared
permanently out of scope by
[ADR-0003](./adr/0003-compose-out-of-scope.md) — orchestration (`deploy`),
cross-VM namespace sharing (`ipc:`/`pid:`/`network_mode: host`),
Windows-only keys, Docker-platform machinery, and device passthrough — and
the headline conformance score counts only in-scope capabilities. Out-of-
scope keys still warn just as loudly.

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
$ urunc-macos compose --profile debug up
$ COMPOSE_PROFILES=debug,tools urunc-macos compose up
```

`compose config` applies the same selection, so it prints exactly the
services `compose up` starts. The whole file is still validated, including
the services the profiles disabled: an error in a service you did not
activate is still an error.

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
$ urunc-macos compose up db-migrations
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

Two cases fail the load instead of picking a winner in silence:

- A service that is defined in an included file and in the including file.
  Rename one of the two services.
- An include entry that lists more than one path. Docker treats the first
  file as a base and the rest as overrides. That merge is not implemented, so
  write one file per entry.

## One-shot services, exit status, and restart policies

`depends_on: { condition: service_completed_successfully }` works, and so
does the `x-oneshot: true` marker that names a job directly — a service
targeted by that condition is a job whether or not it says so. A job boots,
runs its `command` through the guest agent, records the **exact exit code**,
and stops; code 0 releases its dependents, a non-zero code fails `up` with
the code and the job's output and starts nothing that waited on it. Two
consequences worth knowing before you write one:

- A job **requires an agent-bearing image** (one shipping `/urunit-agent`).
  There is no other channel that carries a guest process's exit status
  today, so `up` fails loudly naming the requirement rather than assuming
  success ([ADR-0007](./adr/0007-compose-exit-status-oneshot-restart.md)).
- A job's VM runs a benign init and the command runs through the agent, so
  the job's output is **not** in `compose logs` — it is captured and printed
  when the job fails.

```yaml
services:
  migrate:
    image: harbor.nbfc.io/nubificus/my-migrator:aarch64   # ships /urunit-agent
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
exit cannot be told from a failure — the `:N` attempt cap is still honored);
`always` and `unless-stopped` are indistinguishable here; and a job is never
restarted whatever its policy says. An explicit `urunc-macos stop` outranks
every policy: a service you stop **stays stopped**.

Only a job records an exit code. A plain service whose VM ended on its own
reports `-` in `urunc-macos ps`'s EXIT column — an honest "ended without a
reportable status", and exactly the gap the ADR's Phase B closes. `compose
ps` shows the state word only; a recorded code is visible through
`urunc-macos ps` and `urunc-macos inspect`.

## Example

```yaml
# compose.yaml
services:
  db:
    image: harbor.nbfc.io/nubificus/postgres-urunc:aarch64
    environment:
      POSTGRES_PASSWORD: dev
    mem_limit: 1g
    x-healthcheck-tcp:
      port: 5432
      interval: 1s
      timeout: 30s

  api:
    image: harbor.nbfc.io/nubificus/my-api:aarch64
    command: ["/usr/local/bin/api", "--db", "postgres://db:5432"]
    depends_on:
      db:
        condition: service_healthy
    ports:
      - "8080:8080"
    volumes:
      - ./config:/etc/api
    cpus: 2
```

```bash
urunc-macos compose up          # boots db, waits for :5432, then api
urunc-macos compose ps
curl localhost:8080             # forwarded to api:8080 through the gateway
urunc-macos compose logs -f api
urunc-macos compose down        # stops both VMs and the gateway
```

Here `api` reaches the database at `db:5432` by name, `up` blocks on the
Postgres TCP healthcheck before starting `api`, and host `:8080` is
forwarded to the `api` guest.

## Limits

- One service = one VM: a stack of N services boots N VMs (the subnet caps
  the count; a `/24` gives ~244 usable service addresses).
- `--subnet` must be large enough for the services (`up` errors if not).
- `build:` is unsupported — pre-build and push images (see
  [`NOFireAI/urunc-images`](https://github.com/NOFireAI/urunc-images) for the pattern).
