# Compose-spec conformance

<!-- GENERATED FILE — DO NOT EDIT BY HAND. -->
<!-- Regenerate with `make conformance-report`; edit test/conformance/capabilities.yaml instead. -->

This report is generated from [`test/conformance/capabilities.yaml`](../test/conformance/capabilities.yaml) by `go run ./test/conformance/cmd/report`. Do not edit it by hand — change the manifest and run `make conformance-report`.

Measured against the pinned compose-spec schema snapshot (commit `4e2fe7602af8c965ab4fef891e9dde9c5940775f`, fetched 2026-07-29); see [`test/conformance/spec/PROVENANCE.md`](../test/conformance/spec/PROVENANCE.md).

## Overall: 17.1% of in-scope capabilities

`score = (supported + 0.5 × partial) / in-scope total` over 114 in-scope capabilities: **9 supported**, **21 partial**, **84 unsupported**.

Full-spec coverage: **14.4%** of all 135 capabilities; 21 are declared out of scope by [ADR-0003](adr/0003-compose-out-of-scope.md) (structurally foreclosed: orchestration, cross-VM namespace sharing, Windows-only, Docker-platform machinery, device passthrough). Out-of-scope keys still warn loudly and stay tested.

**Tier** — `static` entries are verified in CI without booting a VM; `runtime` entries are verified only where the runtime suite runs (`HULL_CONFORMANCE_RUNTIME=1`, self-hosted runner or a developer Mac).

## Scores by area

Scores are over in-scope entries; the out-of-scope column is excluded from the denominator.

| Area | Supported | Partial | Unsupported | Out of scope | Score |
|------|-----------|---------|-------------|--------------|-------|
| build | 0 | 0 | 1 | 0 | 0.0% |
| cli | 0 | 7 | 21 | 1 | 12.5% |
| config | 3 | 1 | 4 | 0 | 43.8% |
| deploy | 0 | 0 | 0 | 2 | — |
| develop | 0 | 0 | 1 | 0 | 0.0% |
| extensions | 1 | 2 | 0 | 0 | 66.7% |
| lifecycle | 0 | 5 | 8 | 0 | 19.2% |
| metadata | 0 | 0 | 3 | 0 | 0.0% |
| models | 0 | 0 | 0 | 2 | — |
| networking | 0 | 1 | 10 | 3 | 4.5% |
| resources | 0 | 2 | 17 | 4 | 5.3% |
| security | 0 | 0 | 12 | 6 | 0.0% |
| services | 5 | 1 | 5 | 2 | 50.0% |
| storage | 0 | 0 | 0 | 1 | — |
| volumes | 0 | 2 | 2 | 0 | 25.0% |

## Out of scope (ADR-0003)

These capabilities are structurally foreclosed, not backlog. They still warn loudly and keep their conformance tests.

| Capability | Area | Category | Notes |
|------------|------|----------|-------|
| `cli.scale` | cli | orchestration | No 'compose scale' subcommand; one VM per service. Out of scope (ADR-0003): orchestration. |
| `service.cpu_count` | resources | Windows containers only | Only 'cpus' maps to vCPUs; cpu_count is ignored. Out of scope (ADR-0003): Windows containers only. |
| `service.cpu_percent` | resources | Windows containers only | CPU percentage is not modeled; ignored. Out of scope (ADR-0003): Windows containers only. |
| `service.credential_spec` | security | Windows containers only | Windows managed-service-account credential specs are not applicable; ignored. Out of scope (ADR-0003): Windows containers only. |
| `service.deploy` | deploy | orchestration | Swarm deploy config (replicas, placement, update/rollback, resources) is not honored; use 'cpus'/'mem_limit' for resources. Out of scope (ADR-0003): orchestration. |
| `service.devices` | resources | no device passthrough | Host device passthrough is not implemented; ignored. Out of scope (ADR-0003): no device passthrough. |
| `service.external_links` | networking | cross-VM namespace sharing | Links to containers started outside the project are not supported. Out of scope (ADR-0003): cross-VM namespace sharing. |
| `service.gpus` | resources | no device passthrough | GPU passthrough is not available; ignored. Out of scope (ADR-0003): no device passthrough. |
| `service.ipc` | security | cross-VM namespace sharing | IPC-namespace sharing does not apply across independent VMs; ignored. Out of scope (ADR-0003): cross-VM namespace sharing. |
| `service.isolation` | security | Windows containers only | Isolation-technology selection is not applicable; ignored. Out of scope (ADR-0003): Windows containers only. |
| `service.links` | networking | cross-VM namespace sharing | Legacy container links are superseded by name-based discovery on the project subnet; ignored. Out of scope (ADR-0003): cross-VM namespace sharing. |
| `service.models` | models | Docker-platform machinery | Referencing top-level AI models is not supported; ignored. Out of scope (ADR-0003): Docker-platform machinery. |
| `service.network_mode` | networking | cross-VM namespace sharing | Alternate network modes (host/none/service:/container:) are not supported; every service is on the project subnet. Out of scope (ADR-0003): cross-VM namespace sharing. |
| `service.pid` | security | cross-VM namespace sharing | PID-namespace sharing does not apply across independent VMs; ignored. Out of scope (ADR-0003): cross-VM namespace sharing. |
| `service.provider` | services | Docker-platform machinery | Externally managed provider services are not supported; ignored. Out of scope (ADR-0003): Docker-platform machinery. |
| `service.runtime` | services | Docker-platform machinery | Container runtime selection is not applicable; the VM backend is chosen with the x-hypervisor extension. Out of scope (ADR-0003): Docker-platform machinery. |
| `service.scale` | deploy | orchestration | Replica scaling is not supported; one VM per service. Out of scope (ADR-0003): orchestration. |
| `service.storage_opt` | storage | Windows containers only | Storage-driver options are not applicable; ignored. Out of scope (ADR-0003): Windows containers only. |
| `service.use_api_socket` | security | Docker-platform machinery | Bind-mounting the Docker API socket is not supported; ignored. Out of scope (ADR-0003): Docker-platform machinery. |
| `service.uts` | security | cross-VM namespace sharing | UTS-namespace sharing does not apply across independent VMs; ignored. Out of scope (ADR-0003): cross-VM namespace sharing. |
| `toplevel.models` | models | Docker-platform machinery | Top-level AI model definitions are not supported; ignored. Out of scope (ADR-0003): Docker-platform machinery. |

## Capabilities

### build (0.0%)

| Capability | Status | Tier | Divergence notes | Security tradeoffs |
|------------|--------|------|------------------|--------------------|
| `service.build` | unsupported | static | Image building is out of scope; pre-build and push images ('image:' is required). loadComposeFile errors when a service has no image. | — |

### cli (12.5%)

| Capability | Status | Tier | Divergence notes | Security tradeoffs |
|------------|--------|------|------------------|--------------------|
| `cli.attach` | unsupported | static | No 'compose attach' subcommand; stdio attach is not modeled. | — |
| `cli.build` | unsupported | static | No 'compose build' subcommand; building is out of scope and 'image:' is required. | — |
| `cli.config` | partial | static | Validates and prints the effective config without booting VMs (the static-tier entrypoint; added in #56). Positional SERVICE arguments select the services to render, docker's selection rules: the named services plus their depends_on closure, with a named service's own profiles honored. Divergence: none of docker's config flags (--services, --volumes, --hash, --format, --quiet, --no-interpolate, --resolve-image-digests). | Prints normalized values with interpolation applied, so resolved secrets from the environment and .env reach stdout; avoid running it where its output is captured. |
| `cli.cp` | unsupported | static | No 'compose cp' subcommand; use a bind volume to move files. | — |
| `cli.create` | unsupported | static | No 'compose create' subcommand; 'up' both creates and starts. | — |
| `cli.down` | partial | runtime | Stops and removes every project service in reverse startup order and terminates the gateway. Divergence: no --volumes/--rmi/-t flags. | Best-effort teardown removes the gateway control sockets on success, so no listening endpoint is left behind. |
| `cli.events` | unsupported | static | No 'compose events' subcommand; there is no event stream. | — |
| `cli.exec` | partial | runtime | 'compose exec [-T] [--user] [--env] [--workdir] SERVICE CMD...' resolves the service onto its instance and runs through the guest agent, allocating a tty when stdin is one (-T disables). Divergences: requires an agent-bearing image; no --detach, --privileged or --index (one VM per service, no replicas). | Grants interactive command execution inside the guest to anyone who can reach the instance's agent socket in the store directory; the socket is a plain unix socket protected only by file permissions. |
| `cli.kill` | unsupported | static | No 'compose kill' subcommand; use 'compose down'. | — |
| `cli.logs` | partial | runtime | Prints per-service logs; with no argument, all services are prefixed. --follow (-f) is restricted to a single named service, unlike docker compose which can follow all. | Log lines are echoed verbatim with no redaction, so secrets a service prints to stdout appear in 'compose logs' output. |
| `cli.ls` | unsupported | static | No 'compose ls' subcommand; projects are not enumerated across the store. | — |
| `cli.pause` | unsupported | static | No 'compose pause' subcommand; VM pause/resume is not exposed. | — |
| `cli.port` | unsupported | static | No 'compose port' subcommand; inspect 'ports:' via 'compose config'. | — |
| `cli.ps` | partial | runtime | Lists the project's services with instance, status and IP. Divergences: no --format/-a/-q flags, no JSON output; and the status column is the bare instance state word, so a service that ended reads 'stopped' where docker prints 'Exited (N)'. A recorded exit code (only a one-shot job has one, ADR-0007 section 2) is visible through the top-level 'hull ps' EXIT column and 'hull inspect', not here; TestConformanceRuntime/service.depends_on has a completed job to look at and pins both halves. | none identified |
| `cli.pull` | unsupported | static | No 'compose pull' subcommand; images are pulled during 'up'. (Top-level 'hull pull' exists.) | — |
| `cli.push` | unsupported | static | No 'compose push' subcommand; image publishing is out of scope. | — |
| `cli.restart` | unsupported | static | No 'compose restart' subcommand; run 'compose down' then 'compose up'. | — |
| `cli.rm` | unsupported | static | No 'compose rm' subcommand; 'compose down' removes stopped services. (Top-level 'hull rm' exists.) | — |
| `cli.run` | unsupported | static | No 'compose run' one-off subcommand. (Top-level 'hull run' launches a single instance.) | — |
| `cli.scale` | out-of-scope | static | No 'compose scale' subcommand; one VM per service. Out of scope (ADR-0003): orchestration. | — |
| `cli.start` | unsupported | static | No 'compose start' subcommand; bring the project up with 'compose up'. | — |
| `cli.stats` | unsupported | static | No 'compose stats' subcommand; per-VM resource stats are not collected. | — |
| `cli.stop` | unsupported | static | No 'compose stop' subcommand; use 'compose down'. (Top-level 'hull stop' targets a single instance.) | — |
| `cli.top` | partial | runtime | 'compose top [SERVICE...]' lists guest processes by running /bin/ps through the agent. Divergences: requires an agent-bearing image with /bin/ps; output is the guest's ps format, not docker's UID/PID table. | Process listings can reveal command-line secrets of guest processes to anyone able to invoke the CLI against the store. |
| `cli.unpause` | unsupported | static | No 'compose unpause' subcommand; VM pause/resume is not exposed. | — |
| `cli.up` | partial | runtime | Boots one VM per service and a per-project user-mode gateway. Always detached (-d accepted but ignored); re-running up on an already-up project errors instead of reconciling. --subnet sizes the network. Positional SERVICE arguments start only those services and their depends_on closure, and start a named service whatever its profiles say (TestConformanceStatic/service.profiles pins the selection rules through 'config', which applies them identically). Divergence: --no-deps is not implemented, so the closure is always included. The gateway it starts is also the project supervisor (ADR-0007 section 3), so restart policies keep acting after up has returned: TestConformanceRuntime/cli.up is where that is proven live — a killed 'restart: always' service returns within a bounded wait, and a service stopped with 'hull stop' stays stopped. | Runs unprivileged (no root, no vmnet entitlement). Ctrl-C during up tears down whatever was created, leaving no orphaned gateway. The gateway daemon it leaves behind can boot VMs after up has returned (supervision), bounded by the project's own compose file and by the on-disk Supervise flag that teardown clears before it stops anything. |
| `cli.version` | unsupported | static | No 'compose version' subcommand; 'hull --version' reports the binary version. | — |
| `cli.wait` | unsupported | static | No 'compose wait' subcommand; exit-code capture is not implemented. | — |
| `cli.watch` | unsupported | static | No 'compose watch' subcommand; develop/watch mode is not implemented. | — |

### config (43.8%)

| Capability | Status | Tier | Divergence notes | Security tradeoffs |
|------------|--------|------|------------------|--------------------|
| `service.configs` | unsupported | static | Per-service config mounts are not created; bake config into the image or use a bind volume. | — |
| `service.extends` | supported | static | compose-go resolves 'extends' during load, before hull sees the project: both the short form ('extends: SERVICE', same file) and the long form ('extends: {file, service}', a base service in another file) work. Scalar and list fields the extending service declares (image, command, ...) win over the base's; map fields (environment) merge key by key, the extending service's own entries winning ties. An 'extends' target that does not exist is a load error naming the missing service. | An 'extends: {file: ...}' target is read like any other compose file hull is pointed at, from wherever the path resolves to; it goes through the same size cap and loud-warning walk as a directly named or included file (via compose-go's own "extends" load event, the same mechanism 'include:' uses), so an unsupported key declared only on the extends target does not silently vanish. Scope matches 'include:'s: only an extends used directly in a file passed to Load fires the event, so an extends inside an already-included file, or inside an extends target itself (an extends chain more than one file deep), is not separately capped or warn-walked — compose-go resolves it, but this package's UX layer does not see it. |
| `service.profiles` | supported | static | Profiles select the services that start. A service that declares no profiles always starts; a service that declares one or more starts only when one of them is active. Profiles are activated with the repeatable 'compose --profile NAME' or with COMPOSE_PROFILES (comma-separated), and '*' activates every declared profile. 'config' renders the same set 'up' would start. Cross-service references into a disabled service are still validated (a required depends_on target no active profile enables is an error), but a disabled service's own shape is not: the profile filter runs before compose-go's consistency check, so a disabled service's missing image, for instance, is never seen while it stays disabled. A depends_on target that no active profile enables is an error, never an automatic activation, which is docker's behavior too (docker reports it as "service is required by ... but is disabled"), and docker's escape hatch for it is honored: 'required: false' warns and drops the edge instead of failing. Profile names are not validated, because neither the JSON schema nor compose-go validates them, so a name docker accepts cannot fail here. Naming a service on the command line ('up SERVICE...', 'config SERVICE...') activates that service's own profiles and then reduces to the named services plus their depends_on closure, docker's WithServicesEnabled order. So a dependency sharing the named service's profile comes along, while a service that merely shares that profile and is in no closure stays out. The whole enabled project is validated before the file is narrowed to the named services, so naming one service does not excuse another enabled service's gated dependency. The selection was diffed against docker compose v5.1.1 over a real multi-profile file and a synthetic one covering siblings, gated dependencies and 'required: false'; every case agreed, and no divergence is known. Addition, not a divergence: an active profile that no service declares emits a warning line, where docker is silent. Which services start is unchanged by it. | Selection only ever removes services from the set that boots, so an unrecognized or misspelled profile can never start more than the file declares. A profile name is only ever compared as a string and is never joined onto a path, so leaving names unvalidated adds no filesystem exposure. |
| `spec.interpolation` | partial | static | Substitutes over the parsed YAML scalars, docker's model: comments are inert and values cannot inject structure. All operator forms ($VAR, ${VAR}, :-/-/:?/?/:+/+, $$ escape) over process env then the .env beside the compose file, replaceable with --env-file; unset substitutes empty with a warning, :?/? fail loudly. .env values expand against earlier entries in the same file, and a default nested several levels deep resolves fully — both used to be documented divergences from docker under the bespoke loader; neither is anymore. One divergence remains: a $ before a non-variable character passes through literally where docker errors. | Interpolation reads the caller's process environment and .env files; resolved values (including secrets) appear in 'compose config' output and persist in instance state like all environment values. |
| `toplevel.configs` | unsupported | static | Top-level configs are not created or mounted. Bake configuration into the image or supply it through a bind volume. | — |
| `toplevel.include` | supported | static | Included files are merged into the project: the short form (a path) and the long form (path, project_directory, env_file) both work, and an included file can include more files. Relative paths inside an included file resolve against that file's directory, or against its 'project_directory' when the include sets one, so 'config' and 'up' mount the same host path. Each included file is interpolated with its own environment: the include's 'env_file' when given, else the .env in the directory that file's relative paths resolve against, which is its own directory unless the include set 'project_directory'; the including file's environment is not inherited. A single include entry that lists several paths merges them, later overriding earlier, docker's own multi-file semantics; a service declared in both the including file and an included one (or in two included files) field-merges the same way a multi-file '-f' merge would, the including/later file's fields winning ties — this and the path-list merge were both bespoke-loader-only divergences (refused outright) that compose-go resolves correctly. Include nests arbitrarily deep (compose-go imposes no depth cap; the bespoke loader's own 32-level cap is gone), and a cycle is reported as a cycle. Unsupported keys inside an included file warn like any other, with the file named. | An include reads any path the file names, so a compose file from elsewhere can pull in files from outside the project directory, exactly as docker compose does. What it can do with them is bounded by the same loader: the included services go through the same validation, and a volume name they declare still cannot escape the volumes root — not because of a urunc-side check (there is none anymore), but because compose-go's own JSON-schema pattern for a volume name ([a-zA-Z0-9._-]+) already rules out '/' and '..', and namedVolumeDir always joins the name onto '<project>_<name>', so traversal is unreachable by construction. |
| `toplevel.name` | unsupported | static | The compose-file 'name:' key is not read. The project name comes from -p / COMPOSE_PROJECT_NAME / the working-directory name instead. | — |
| `toplevel.version` | unsupported | static | The deprecated top-level 'version:' key is ignored, matching docker compose v2 which also ignores it. | — |

### deploy (—)

| Capability | Status | Tier | Divergence notes | Security tradeoffs |
|------------|--------|------|------------------|--------------------|
| `service.deploy` | out-of-scope | static | Swarm deploy config (replicas, placement, update/rollback, resources) is not honored; use 'cpus'/'mem_limit' for resources. Out of scope (ADR-0003): orchestration. | — |
| `service.scale` | out-of-scope | static | Replica scaling is not supported; one VM per service. Out of scope (ADR-0003): orchestration. | — |

### develop (0.0%)

| Capability | Status | Tier | Divergence notes | Security tradeoffs |
|------------|--------|------|------------------|--------------------|
| `service.develop` | unsupported | static | Watch/develop mode is not implemented; ignored. | — |

### extensions (66.7%)

| Capability | Status | Tier | Divergence notes | Security tradeoffs |
|------------|--------|------|------------------|--------------------|
| `ext.x-healthcheck-tcp` | supported | runtime | Bare-port or struct ({port, interval, retries, start_period}) form. Gates depends_on 'service_healthy'. A plain TCP connect: proves a listener exists, not application readiness. | Probes are issued by the per-project gateway through its local HTTP API socket over the private subnet; no probe traffic leaves the host and no guest exec is performed. |
| `ext.x-hypervisor` | partial | static | Selects the guest VMM backend per service ('vz' or 'qemu', plus the aliases qemu-hvf/virtualization/apple); unknown values are rejected at parse time. Divergence: the implemented default is 'vz' when unset, while the docs claim it defaults to the image annotation. | Chooses the hypervisor backend only; both run unprivileged with no entitlements and no root, so backend choice grants no extra host access. |
| `ext.x-oneshot` | partial | static | Marks a service as a job (ADR-0007 section 1): its 'command' runs to completion through the guest agent, the exact exit code is recorded on the instance record, and the VM is stopped once the command returns. The marker is implied as well as declared — a service targeted by any dependent's 'service_completed_successfully' condition is a job too, and 'compose config' renders 'x-oneshot: true' for both. Divergences: a job REQUIRES an agent-bearing image (without /urunit-agent it cannot report a status, so 'up' fails loudly naming the requirement instead of assuming success); a job must declare a 'command', from either direction (x-oneshot without one, or a completion-gated dependency without one, is a load error); the job's VM boots a benign init ('/bin/sleep 86400', so the image must carry /bin/sleep on top of the agent) and the command runs through the agent, so the job's output does NOT appear in 'compose logs' — it is captured and printed only when the job fails, where docker would stream it like any other service; and a job is never restarted whatever its 'restart:' policy says, which warns at load. Marker rendering, both load errors and the ignored-restart warning are pinned by the static case named below; the exit-status behavior the extension exists for — a job exiting 0 releasing its dependents, a non-zero one failing 'up' with the code — needs a booted agent-bearing guest and is proven inside TestConformanceRuntime/service.depends_on. | A job's command executes inside the guest through the agent socket in the store directory, at the same trust level as the service command itself; its captured output is printed by 'up' on failure, so anything the job prints (including secrets) reaches the operator's terminal and CI logs, and the recorded exit code persists in plaintext instance state under the store. |

### lifecycle (19.2%)

| Capability | Status | Tier | Divergence notes | Security tradeoffs |
|------------|--------|------|------------------|--------------------|
| `service.attach` | unsupported | static | stdio attach is not modeled; up runs detached. Use 'compose logs'. | — |
| `service.depends_on` | partial | runtime | List and map forms are accepted. 'service_started', 'service_healthy' and 'service_completed_successfully' are honored; no other condition is. 'service_healthy' requires the dependency to declare a healthcheck (exec form) or x-healthcheck-tcp; the exec form wins when both are present. 'service_completed_successfully' runs the dependency as a job (its command executes through the guest agent) and gates on exit code 0: an image without /urunit-agent cannot report a status, so the up fails loudly instead of assuming success. The gated dependency must declare a 'command' — a job with nothing to run cannot report anything — and a file that omits it fails at load (pinned by TestConformanceStatic/ext.x-oneshot). The gate reads the recorded exit code and nothing else: it is evaluated once, in startup order, and a dependency whose record carries no status is a refusal rather than an assumed success. 'required' is honored and defaults to true. 'required: false' never fails its dependent: an optional dependency that no active profile enables warns and its edge is dropped (TestConformanceStatic/service.profiles), and one that is enabled but never completes or never becomes healthy warns and the dependent starts anyway, as docker does. Divergence: the condition is checked only while 'up' walks the graph, so a dependency that dies later does not retroactively stop its dependents (docker behaves the same way). | A completion-gated dependency runs its command inside the guest through the agent socket, at the same trust level as the service command itself; the job's captured output is printed by 'up' when it fails, so anything the job wrote to stdout or stderr (including secrets) reaches the operator's terminal and CI logs. |
| `service.healthcheck` | partial | runtime | Exec-form probes (test as CMD / CMD-SHELL / NONE, interval, timeout, retries, start_period, disable) run inside the guest through the urunit-agent and gate depends_on 'service_healthy'. Divergences: the image must ship /urunit-agent (a declared healthcheck that cannot run fails 'up' loudly instead of passing); health state is only evaluated while 'up' waits on a dependency, not continuously; when both healthcheck and x-healthcheck-tcp are declared the exec form wins. | Probe commands execute inside the guest as the image's configured user (root when unset) via the agent socket, and their output is captured to the host process; a malicious compose file can run arbitrary commands in the guest it defines, which is the same trust level as the service command itself. |
| `service.init` | unsupported | static | Init-process injection is not applicable; urunit runs as PID 1 in the guest. | — |
| `service.logging` | unsupported | static | Logging drivers are not configurable; output goes to a per-instance log file read by 'compose logs'. | — |
| `service.post_start` | partial | runtime | Runs after the service starts, through the guest agent (command, user, environment). Divergences: requires an agent-bearing image; a failed hook fails 'up' and tears the project down; privileged and working_dir are not honored and warn. | Hook commands execute inside the guest with the requested user (the image's user when unset); same trust level as the service command. |
| `service.pre_start` | unsupported | static | pre_start init containers are not executed; the compose layer does not drive the instance-level exec facility for lifecycle hooks yet. | — |
| `service.pre_stop` | partial | runtime | Runs before each service stops during 'compose down', through the guest agent (command, user, environment). Divergences: requires an agent-bearing image; a failed or unreachable hook warns and the stop proceeds (down must always work); requires the original compose file to still exist at its recorded path. | Hook commands execute inside the guest with the requested user (the image's user when unset); same trust level as the service command. |
| `service.restart` | partial | static | 'no \| always \| on-failure[:N] \| unless-stopped' are parsed, validated (an unknown value, and a non-numeric ':N', are load errors naming the accepted set) and honored by the per-project supervisor inside the gateway daemon, which polls instance liveness and re-runs a service that disappeared with capped exponential backoff. 'compose config' renders the policy as declared, not the mode it degrades to, and renders nothing when the file declared nothing. Divergences (ADR-0007 section 3): a restart is noticed within the poll interval, not instantly; 'on-failure' degrades to 'always' with a load-time warning, because no exit code is observable for a plain service, so a clean exit cannot be told from a failure (the ':N' attempt cap is still honored); an explicit 'hull stop' outranks EVERY policy through the StoppedByUser marker (ADR-0007 section 2), so 'always' and 'unless-stopped' are indistinguishable here and both behave like docker's 'unless-stopped' — docker's 'always' would bring a manually stopped container back when the daemon restarts, this supervisor never does; and a one-shot service (job) is never restarted whatever its policy says, which warns at load. A restarted service is re-run alone, so its depends_on gates are not re-evaluated and it can come back while a dependency is down (docker behaves the same way). Enforcement needs the project's gateway daemon: a hand-started gateway without --project parses the policy and supervises nothing. Parsing, validation, the degrade warning and rendering are pinned by the static case named below; the supervision loop's own effects — a killed service returning, a stopped one staying stopped — need a booted project and are proven inside TestConformanceRuntime/cli.up. | A restart re-runs the service from the project's own persisted state and compose file, as the user that ran 'up', with the service's CURRENT definition: the supervisor re-reads the compose file each poll, so a file edited after 'up' takes effect on the next restart, while the gateway's port forwards stay as 'up' set them (a service given a new port comes back unreachable). It grants no privilege the first start did not have. Restart storms are bounded by the capped exponential backoff, so a crash-looping guest cannot be used to spin host resources. The supervisor refuses to start anything once the project's supervision flag is cleared or its state is removed, so teardown cannot be undone by a daemon that outlives it. |
| `service.stdin_open` | unsupported | static | Interactive stdin is not modeled; up is always detached. | — |
| `service.stop_grace_period` | unsupported | static | Per-service stop grace is not read; the runtime uses a fixed SIGTERM-then-SIGKILL grace (run --stop-grace, default 10s). | — |
| `service.stop_signal` | unsupported | static | Custom stop signal is not configurable; SIGTERM is sent to the VM supervisor. | — |
| `service.tty` | unsupported | static | PTY allocation is not modeled; ignored. | — |

### metadata (0.0%)

| Capability | Status | Tier | Divergence notes | Security tradeoffs |
|------------|--------|------|------------------|--------------------|
| `service.annotations` | unsupported | static | OCI annotations are not applied to the guest; ignored. | — |
| `service.label_file` | unsupported | static | Labels themselves are never applied to a running service, so label_file stays unsupported: the key still warns as unsupported. Behavior changed under the hood: compose-go reads and parses the named file during load, to resolve the service's Labels, whether or not hull does anything with them; a label_file that does not exist now fails the load with compose-go's own error, where the bespoke loader never looked at the file at all. Previously-optional decoration is now a hard requirement for the file to exist and parse. | — |
| `service.labels` | unsupported | static | Container labels are not applied; ignored. | — |

### models (—)

| Capability | Status | Tier | Divergence notes | Security tradeoffs |
|------------|--------|------|------------------|--------------------|
| `service.models` | out-of-scope | static | Referencing top-level AI models is not supported; ignored. Out of scope (ADR-0003): Docker-platform machinery. | — |
| `toplevel.models` | out-of-scope | static | Top-level AI model definitions are not supported; ignored. Out of scope (ADR-0003): Docker-platform machinery. | — |

### networking (4.5%)

| Capability | Status | Tier | Divergence notes | Security tradeoffs |
|------------|--------|------|------------------|--------------------|
| `service.dns` | unsupported | static | Custom DNS servers are not configurable; the project gateway is the resolver. | — |
| `service.dns_opt` | unsupported | static | Custom DNS resolver options are not passed to the guest; ignored. | — |
| `service.dns_search` | unsupported | static | Custom DNS search domains are not set; ignored. | — |
| `service.domainname` | unsupported | static | Guest domainname is not set; ignored. | — |
| `service.expose` | unsupported | static | expose is a no-op: the private subnet already permits service-to-service traffic on any port, so nothing needs exposing. Ignored. | — |
| `service.external_links` | out-of-scope | static | Links to containers started outside the project are not supported. Out of scope (ADR-0003): cross-VM namespace sharing. | — |
| `service.extra_hosts` | unsupported | static | Additional /etc/hosts entries are not injected beyond the automatic service-name to IP map the gateway installs. | — |
| `service.hostname` | unsupported | static | The hostname key is ignored; hull does not set a guest hostname (whatever the image/kernel chooses applies). Services are still reachable by their compose name via the gateway. | — |
| `service.links` | out-of-scope | static | Legacy container links are superseded by name-based discovery on the project subnet; ignored. Out of scope (ADR-0003): cross-VM namespace sharing. | — |
| `service.mac_address` | unsupported | static | A declared MAC is ignored; the runtime assigns a deterministic MAC per instance for lease-based IP capture. | — |
| `service.network_mode` | out-of-scope | static | Alternate network modes (host/none/service:/container:) are not supported; every service is on the project subnet. Out of scope (ADR-0003): cross-VM namespace sharing. | A guest cannot join the host network namespace; 'network_mode: host' is inert, which is a security-positive default. |
| `service.networks` | unsupported | static | Per-service network attachment and configuration are not supported; every service sits on the single flat project subnet (see --subnet). | — |
| `service.ports` | partial | runtime | TCP only; [HOSTIP:]HOST:GUEST forms only (single-port, ranges and /udp are rejected with targeted errors). Publishing binds 127.0.0.1 by default rather than Docker's 0.0.0.0. | The default 127.0.0.1 bind is a security-positive divergence from Docker: a published port is reachable off-host only when an explicit HOSTIP prefix requests it. |
| `toplevel.networks` | unsupported | static | Top-level network definitions are not honored. One flat project subnet is used; size it with 'compose up --subnet CIDR'. | — |

### resources (5.3%)

| Capability | Status | Tier | Divergence notes | Security tradeoffs |
|------------|--------|------|------------------|--------------------|
| `service.blkio_config` | unsupported | static | Block-IO limits are not passed to the VM backend; ignored. | — |
| `service.cgroup_parent` | unsupported | static | Host cgroup placement of the guest is not modeled; ignored. | — |
| `service.cpu_count` | out-of-scope | static | Only 'cpus' maps to vCPUs; cpu_count is ignored. Out of scope (ADR-0003): Windows containers only. | — |
| `service.cpu_percent` | out-of-scope | static | CPU percentage is not modeled; ignored. Out of scope (ADR-0003): Windows containers only. | — |
| `service.cpu_period` | unsupported | static | CFS CPU period tuning is not applicable to whole-vCPU allocation; ignored. | — |
| `service.cpu_quota` | unsupported | static | CFS CPU quota tuning is not applicable to whole-vCPU allocation; ignored. | — |
| `service.cpu_rt_period` | unsupported | static | Real-time CPU period tuning is not applicable; ignored. | — |
| `service.cpu_rt_runtime` | unsupported | static | Real-time CPU runtime tuning is not applicable; ignored. | — |
| `service.cpu_shares` | unsupported | static | Relative CPU shares do not map to whole-vCPU allocation; ignored. Use 'cpus'. | — |
| `service.cpus` | partial | static | Number and quoted-string forms are accepted. A fractional value is rounded UP to a whole vCPU (only whole vCPUs are allocatable), with a warning printed. 'compose config' renders the rounded count, matching what 'up' passes as --cpus. Docker renders the declared fraction instead, but docker can deliver a fractional CPU share and urunc cannot, so reporting the declared value would misstate what the VM gets. | Rounding fractional cpus up over-allocates host CPU versus the declared limit; a dense stack can oversubscribe host cores. |
| `service.cpuset` | unsupported | static | CPU pinning is not exposed by the VM backends; ignored. | — |
| `service.device_cgroup_rules` | unsupported | static | Device cgroup rules do not apply to a VM guest; ignored. | — |
| `service.devices` | out-of-scope | static | Host device passthrough is not implemented; ignored. Out of scope (ADR-0003): no device passthrough. | — |
| `service.gpus` | out-of-scope | static | GPU passthrough is not available; ignored. Out of scope (ADR-0003): no device passthrough. | — |
| `service.mem_limit` | partial | static | m/mb, g/gb, k/kb and b suffixes, and plain byte counts, are all accepted (compose-go's unit vocabulary is wider than the bespoke parser's, which rejected k/b as "too small"). 'compose config' renders a byte count as a quoted string (e.g. 1g renders "1073741824"), not a normalized "<N>m" value. The value that reaches the VM backend is bytes / 1048576, floor division, and 'config' renders that same floored amount so the two cannot drift: a declared 3670016 (3.5 MiB) renders "3145728", the 3 MB the VM actually gets. A value under 1 MiB would floor to 0, so 'validateProject' rejects it at load/validate time instead, naming the service and the byte value. | A mem_limit under 1 MiB is rejected before it ever reaches the run layer, so '--mem 0' is not reachable through this path; the error names the service and points at the compose file, not an opaque VM boot failure. |
| `service.mem_reservation` | unsupported | static | Soft memory reservation is not modeled; use 'mem_limit'. | — |
| `service.mem_swappiness` | unsupported | static | Guest swappiness is not tunable; ignored. | — |
| `service.memswap_limit` | unsupported | static | Swap limits are not modeled; ignored. | — |
| `service.oom_kill_disable` | unsupported | static | OOM-killer control is not modeled; ignored. | — |
| `service.oom_score_adj` | unsupported | static | OOM score adjustment is not modeled; ignored. | — |
| `service.pids_limit` | unsupported | static | PID limits are not enforced; ignored. | — |
| `service.shm_size` | unsupported | static | /dev/shm sizing is not configurable; ignored. | — |
| `service.ulimits` | unsupported | static | ulimits are not applied to the guest; ignored. | — |

### security (0.0%)

| Capability | Status | Tier | Divergence notes | Security tradeoffs |
|------------|--------|------|------------------|--------------------|
| `service.cap_add` | unsupported | static | Linux capabilities are a container concept and do not apply to a VM guest; ignored. | The guest is a VM, not a namespaced container sharing the host kernel, so added capabilities grant no host-level privilege. |
| `service.cap_drop` | unsupported | static | Capability drops do not apply to a VM guest; ignored. | Dropping capabilities is a no-op here; guest-to-host isolation comes from the VM boundary, not the capability set. |
| `service.cgroup` | unsupported | static | cgroup-namespace selection does not apply to a VM guest; ignored. | none identified |
| `service.credential_spec` | out-of-scope | static | Windows managed-service-account credential specs are not applicable; ignored. Out of scope (ADR-0003): Windows containers only. | — |
| `service.group_add` | unsupported | static | Supplementary groups are not applied to the guest process; ignored. | none identified |
| `service.ipc` | out-of-scope | static | IPC-namespace sharing does not apply across independent VMs; ignored. Out of scope (ADR-0003): cross-VM namespace sharing. | Guests cannot share an IPC namespace with the host or each other; the key is inert, a security-positive default. |
| `service.isolation` | out-of-scope | static | Isolation-technology selection is not applicable; ignored. Out of scope (ADR-0003): Windows containers only. | — |
| `service.pid` | out-of-scope | static | PID-namespace sharing does not apply across independent VMs; ignored. Out of scope (ADR-0003): cross-VM namespace sharing. | A guest cannot join the host PID namespace; the key is inert. |
| `service.privileged` | unsupported | static | Privileged mode does not apply to a VM guest; ignored. | A VM guest cannot escalate to host privileges via 'privileged'; the flag is inert, which is a security-positive default. |
| `service.read_only` | unsupported | static | A read-only rootfs is not enforced; the guest boots a writable rootfs clone. | read_only is not honored, so a compromised guest can modify its own per-instance rootfs clone; the host and other guests are unaffected. |
| `service.secrets` | unsupported | static | Per-service secret mounts are not created; no secret file is delivered to the guest. | Silently dropping secrets can push an app to an insecure fallback; and any secret placed in 'environment' instead persists in plaintext state JSON under the store. |
| `service.security_opt` | unsupported | static | seccomp/apparmor/label options do not apply to a VM guest; ignored. | Host-kernel LSM/seccomp policies do not gate a VM guest; isolation is provided by the hypervisor boundary instead. |
| `service.sysctls` | unsupported | static | Guest kernel sysctls are not set; ignored. | none identified |
| `service.use_api_socket` | out-of-scope | static | Bind-mounting the Docker API socket is not supported; ignored. Out of scope (ADR-0003): Docker-platform machinery. | Not honoring use_api_socket avoids exposing a host Docker daemon socket to the guest, a security-positive default. |
| `service.user` | unsupported | static | Process user override is not applied; the image's user is used. | Not applying 'user' means the process runs as the image's declared user inside the guest; it does not affect host-side privileges. |
| `service.userns_mode` | unsupported | static | User-namespace remapping does not apply to a VM guest; ignored. | Guest UIDs never map onto host UIDs (VM boundary), so userns_mode has no host-side effect. |
| `service.uts` | out-of-scope | static | UTS-namespace sharing does not apply across independent VMs; ignored. Out of scope (ADR-0003): cross-VM namespace sharing. | A guest cannot share the host UTS namespace; the key is inert. |
| `toplevel.secrets` | unsupported | static | Top-level secrets are not created and no secret is delivered to guests. | Secret sources (file/environment) are silently not provisioned; an app relying on a mounted secret may fall back to an insecure default. |

### services (50.0%)

| Capability | Status | Tier | Divergence notes | Security tradeoffs |
|------------|--------|------|------------------|--------------------|
| `service.command` | supported | static | — | The string form is shell-word split on the host with shlex (no host shell is invoked) and becomes the guest process args; the only exposure is running the operator-chosen command inside the guest VM. |
| `service.container_name` | partial | static | sanitizeName restricts names to [a-z0-9_-]; uppercase letters and dots, which the spec pattern permits, are rejected with an error rather than silently renamed. | The name derives on-host state paths under the store; the [a-z0-9_-] restriction blocks path traversal via a crafted container_name. |
| `service.entrypoint` | unsupported | static | Entrypoint override is not read; the image ENTRYPOINT is used as-is. Use 'command' to override the process arguments. | — |
| `service.env_file` | supported | static | String, list, and long ({path, required}) forms; dotenv format with comments and quoting; paths resolve against the compose file's directory; a missing required file fails at load. Precedence is docker's: 'environment' beats env_file, later files beat earlier. | Env-file contents are read by the host, merged into the guest environment, and persist in plaintext instance state like all environment values; 'compose config' prints them resolved. |
| `service.environment` | supported | static | Map and list (KEY=VAL) forms are both accepted and both normalize to a sorted KEY: VAL mapping in 'compose config' output. A bare/null key (map form) or a bare name with no '=' (list form) inherits the host process's value when one is set, and renders null/empty when it is not — matching docker; the bespoke loader's own divergence here (never inheriting) is gone under compose-go. | Values are passed as --env and persisted verbatim in the instance state JSON (CmdLine) under the store in plaintext; anyone with store read access can recover secrets placed in environment. |
| `service.image` | supported | static | — | The reference is pulled by the registry client and runs as guest code; no digest pinning is enforced, so a mutable or typosquatted tag can substitute the image. |
| `service.platform` | unsupported | static | Target platform selection is not honored; the image's native architecture is used. | — |
| `service.provider` | out-of-scope | static | Externally managed provider services are not supported; ignored. Out of scope (ADR-0003): Docker-platform machinery. | — |
| `service.pull_policy` | unsupported | static | Pull policy is not honored; the image is pulled only when absent from the store. | — |
| `service.pull_refresh_after` | unsupported | static | Time-based image refresh (pull_policy=refresh) is not honored. | — |
| `service.runtime` | out-of-scope | static | Container runtime selection is not applicable; the VM backend is chosen with the x-hypervisor extension. Out of scope (ADR-0003): Docker-platform machinery. | — |
| `service.working_dir` | unsupported | static | Working-directory override is not applied; the image's WorkingDir is used. | — |
| `toplevel.services` | supported | static | — | Each service is a separate VM with its own writable rootfs clone; the blast radius of a compromised service is its own guest plus whatever host paths and ports that service explicitly declares. |

### storage (—)

| Capability | Status | Tier | Divergence notes | Security tradeoffs |
|------------|--------|------|------------------|--------------------|
| `service.storage_opt` | out-of-scope | static | Storage-driver options are not applicable; ignored. Out of scope (ADR-0003): Windows containers only. | — |

### volumes (25.0%)

| Capability | Status | Tier | Divergence notes | Security tradeoffs |
|------------|--------|------|------------------|--------------------|
| `service.tmpfs` | unsupported | static | tmpfs mounts are not created; ignored. | — |
| `service.volumes` | partial | runtime | Bind mounts and declared named volumes, short syntax only; named sources resolve to store-managed directories and must be declared top-level (undeclared names fail at load). Relative host paths are rewritten against the compose file's directory only when they start with '.'. ':ro' is accepted but NOT enforced (mounted read-write; a warning is emitted); the guest path must be absolute. Anonymous volumes and the long syntax are rejected. | The host filesystem is exposed to the guest over virtiofs; read-only intent is not enforced, so a compromised guest can write to any bound host path. |
| `service.volumes_from` | unsupported | static | Mounting volumes from another service/container is not supported; ignored. | — |
| `toplevel.volumes` | partial | static | Bare named-volume declarations are honored: each becomes a store-managed host directory (<store>/volumes/<project>_<name>, docker's naming) mounted over virtiofs, persisting across down/up and removed only by 'down --volumes'. Divergences: declaration options (driver, driver_opts, external, labels, name) warn and are ignored, so an 'external: true' volume IS created and IS removed by 'down --volumes' (Docker never removes external volumes); services sharing a volume see host-filesystem semantics through independent virtiofs devices, not Docker's single-kernel page-cache coherence; volume identity is the directory name, so on a case-insensitive filesystem two names differing only in case are one volume, and a project/volume pair that concatenates to another project's pair shares its directory (Docker distinguishes both via volume labels); a volume directory orphaned by a failed 'up' or a moved compose file has no reclamation command. The declaration contract is pinned by the static case named below; the data lifecycle (persistence across down/up, removal by 'down --volumes') is proven inside TestConformanceRuntime/service.volumes. | Named volumes are plain host directories under the store: readable and writable by any process with store access and by every service that mounts them; contents persist in plaintext with no quota, and 'down --volumes' is the only deletion path. |

