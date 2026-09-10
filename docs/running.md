# Running a workload

This page walks from a first boot to a running instance you can exec into and
clean up. It assumes hull is installed; see [install.md](install.md).

Commands here run from any directory. hull keeps no per-project state outside
its store, except for compose projects.

## Two kinds of image

hull boots both, and the difference decides which backends work:

- **A plain container image**, such as `ubuntu:latest`, carries no kernel. hull
  supplies a generic arm64 Linux kernel and an initrd, and boots the image's
  filesystem inside the VM. This works on `vz` and `hvi` only.
- **An image that carries its own kernel**, such as one packaged as a
  unikernel. hull boots the kernel the image carries. This works on all three
  backends.

Not every OCI image is a unikernel, and hull does not turn one into the other.

## First boot

```bash
hull run --hypervisor vz ubuntu:latest /bin/echo hello-from-vz
```

Output:

```
hello-from-vz
```

Pass `--hypervisor` explicitly. Without it, hull reads the image's
`com.urunc.unikernel.hypervisor` annotation, and with no annotation it falls
back to `qemu`, which is not installed by default and cannot boot a plain
image.

### What the first run does that later runs skip

1. Creates the store. hull makes a case-sensitive APFS sparse image and mounts
   it at `~/.hull/store`. See [storage.md](storage.md) for why.
2. Pulls `ubuntu:latest` into the store's image cache.
3. Downloads the generic boot assets, a kernel and an initrd, from the public
   OCI artifact `ghcr.io/nofireai/hull-assets`. The pull is anonymous and needs
   no login.

To do step 3 ahead of time, or on a host that will later be offline:

```bash
hull assets pull          # fetch for this platform
hull assets show          # where they are, and whether they are present
hull assets dir           # just the directory, for scripts
```

If `hull store detach` has run, `hull assets show` reports the assets as
missing, because it only reads a path and will not mount a volume to answer a
question. `hull assets pull` remounts and finds them already there.

### Overriding the command

Everything after the image reference replaces the image's `Cmd`, the way
`docker run` does:

```bash
hull run --hypervisor vz ubuntu:latest /bin/cat /etc/os-release
```

The image entrypoint is kept when that entrypoint is `urunit`.

## A long-running instance

```bash
ID=$(hull run -d --hypervisor vz --net shared ubuntu:latest sleep 600)
echo "$ID"
```

`-d` detaches and prints the instance id. Use it for anything you want to
inspect or exec into.

`--net shared` gives the guest NAT on `vz`. It does **not** give egress on
`hvi`. Read [networking.md](networking.md) before you run a server.

### Look at it

```bash
hull ps
```

Columns are ID, STATUS, EXIT, PID, IP and CREATED. `hull ps` reconciles as it
prints: a record marked running whose process is gone is rewritten to stopped.
There is no JSON mode.

```bash
hull inspect "$ID"        # always JSON
hull logs "$ID"
hull logs -f "$ID"        # follow
hull logs -n 50 "$ID"     # last 50 lines
```

A log file exists only for a detached run. A foreground run gets your terminal
instead, so `hull logs` on one reports the file as not found.

CAUTION: The log is the guest console at mode 0600. It carries whatever the
workload prints, which for an agent-style workload can include tokens it was
given and contents of files it read.

### Run a command inside it

```bash
hull exec "$ID" /bin/cat /etc/os-release
hull exec -t "$ID" /bin/sh
hull exec -u nobody -w /tmp "$ID" /bin/pwd
```

| Flag | What it does |
|---|---|
| `-t`, `--tty` | allocate a pseudo-terminal |
| `-u`, `--user` | guest user |
| `--cwd` | working directory in the guest |
| `-e`, `--env` | `KEY=VALUE`, or a bare `KEY` to inherit from the host |

`hull exec` propagates the guest command's exit status as its own. A foreground
`hull run` does not; see [cli.md](cli.md#exit-statuses).

`exec` works on all three backends, but on `qemu` it needs a 9pfs root, a
`urunit` entrypoint and `/urunit-agent` in the image. See
[backends.md](backends.md).

### A started process is not a ready guest

`hull run -d` returns once the runner process is up. That is not the same as
the guest being ready to accept an `exec` or serve a request. If your script
needs readiness, poll for it:

```bash
until hull exec "$ID" /bin/true 2>/dev/null; do sleep 0.2; done
```

With `--detach` and NAT networking, `--wait-ip` makes hull wait for the DHCP
lease and record the address before returning. That is an address, not
readiness either.

## Environment variables

```bash
hull run --hypervisor vz -e GREETING=hello ubuntu:latest /bin/sh -c 'echo $GREETING'
hull run --hypervisor vz -e HOME_TOKEN ubuntu:latest /bin/sh -c 'echo $HOME_TOKEN'
```

A bare `-e KEY` inherits the value from hull's own environment and keeps it out
of `argv`. A bare `-e KEY` whose variable is unset is dropped rather than
forwarded empty, so it cannot shadow what the image configured.

CAUTION: Keeping a value out of `argv` is not the same as keeping it secret.
The value is written in plaintext to a file inside the instance's rootfs or
initrd, and it stays there until `hull rm`. See
[storage.md](storage.md#credentials-on-disk-read-this-before-forwarding-a-secret).

## Sharing a host directory

```bash
mkdir -p /tmp/hull-demo && echo seeded > /tmp/hull-demo/host.txt

hull run --hypervisor vz \
  --shared-dir /tmp/hull-demo:/work:rw \
  ubuntu:latest /bin/sh -c 'cat /work/host.txt && echo written > /work/guest.txt'

cat /tmp/hull-demo/guest.txt
```

Output:

```
seeded
written
```

Shares are repeatable. Read-only works on `vz` and `hvi` and is refused on
`qemu`. Ownership and setuid behavior differ per backend; see
[storage.md](storage.md#shared-host-directories).

## Sizing

```bash
hull run --hypervisor vz --mem 2048 --cpus 4 ubuntu:latest /bin/sh
```

Defaults are 512 MB and 1 vCPU, which is enough for the examples above and not
enough for much else.

## Stopping and cleaning up

```bash
hull stop "$ID"
hull rm "$ID"
```

`hull stop` signals the runner and deletes nothing. On `vz` it waits
`--stop-grace` seconds, 10 by default, before forcing; a second signal forces
at once. `--stop-grace` has no effect on `qemu`.

`hull rm` deletes exactly `<store>/instances/<id>`: the record, the
per-instance rootfs, the log, the sockets and the checkpoint. It never touches
the image cache, the boot assets or compose volumes, so the next run of the
same image needs no re-pull.

`hull rm` refuses a running instance without `--force`, and removes nothing on
refusal.

To reclaim the whole store:

```bash
hull store detach
rm -rf ~/.hull
```

CAUTION: `rm -rf ~/.hull` destroys every image, instance and checkpoint in the
default store. Nothing recovers it.

There is no `hull rmi` and no `hull prune`. A single cached image can only be
removed by deleting `<store>/images/<digest>` by hand.

## Where to go next

| Next | Page |
|---|---|
| Pick a backend on purpose | [backends.md](backends.md) |
| Give the guest a working network | [networking.md](networking.md) |
| Understand what persists | [storage.md](storage.md) |
| Run several services together | [compose.md](compose.md) |
| Control pull policy, digests and annotations | [images.md](images.md) |
| Fix an error | [troubleshooting.md](troubleshooting.md) |
