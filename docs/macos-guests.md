# macOS guests (experimental)

hull can boot a macOS guest on the Vz backend and treat it like any other
instance: `ps`, `exec`, `logs`, `stop` and `rm` work on it, and `--gui` shows
its display in a window on the host.

A macOS guest is a disk with an identity. There is no OCI image to pull, so
`hull run` takes a guest directory in Tart's layout:

```
config.json   hardware model, machine identifier, sizing, MAC address
disk.img      the installed system
nvram.bin     the auxiliary storage of the boot chain
```

The guest boots from that directory in place. One directory backs one running
instance, and `hull rm` removes the instance record only.

## Run

Pass the directory, or the name of a local Tart VM (`~/.tart/vms/<name>`):

```bash
tart clone ghcr.io/cirruslabs/macos-tahoe-base:latest claude-desktop
hull run -d --gui --net shared claude-desktop
hull exec claude-desktop -- sw_vers
hull exec -t claude-desktop -- zsh
hull stop claude-desktop
```

The instance takes the directory's name unless `--name` is given. `--mem` and
`--cpus` override the sizing in `config.json`. `--net` is `none` (the default)
or `shared`. The flags that describe an OCI image or a Linux guest are refused.

## Passing files and storage to the guest

`--shared-dir` works as it does for a Linux guest, and `--tmpfs` adds a
memory-only volume:

```bash
hull run -d --gui --net shared \
  --shared-dir ~/work:/Users/admin/work \
  --shared-dir ~/reference:/Users/admin/ref:ro \
  --tmpfs /Users/admin/.secrets:256m \
  claude-desktop
```

- A shared directory is a virtiofs share. Writes go straight to the host
  directory, and `:ro` makes the device itself read-only.
- A `--tmpfs` volume is an HFS+ volume on a RAM device in the guest, 64m unless
  a size is given. Nothing written there reaches the guest's disk, and it is
  gone at shutdown.
- Both are owned by whoever owns the directory they are created in, eg. the
  user for a path under `/Users/<name>`.

hull writes the mounts into `mounts.json` in a read-only share tagged
`hull-config`. The guest agent mounts that share at boot and makes each mount,
so this needs `hull-guest-agent` in the guest. The agent log
(`/var/log/hull-guest-agent.log`) has a `guest mounts:` line per mount.

A checkpoint saves guest memory, so it holds whatever a `--tmpfs` volume held
at that moment, in `vm.vzstate` on the host disk. Keep secrets out of a guest
you checkpoint, or treat the checkpoint as a secret.

Single files need no share. The agent passes stdin through, so a tar stream
works in both directions:

```bash
tar -cf - notes/ | hull exec claude-desktop -- tar -xf - -C /Users/admin
hull exec claude-desktop -- tar -cf - -C /Users/admin Documents | tar -xf -
```

## Fast start: checkpoint and restore

A cold boot takes about 20 s to reach a desktop with the application open.
Restoring a checkpoint takes about 4 s, and the guest resumes exactly where it
was saved:

```bash
hull run -d --gui --net shared claude-desktop   # boot once, let the app start
hull checkpoint claude-desktop                  # about 2 s
hull stop claude-desktop
hull restore -d claude-desktop                  # about 4 s, every time
```

A restore rewinds the disk and the auxiliary storage to the checkpoint, so
anything written after it is gone. For a sandbox this is usually what we want:
every restore starts from the same state.

The checkpoint is saved in `<vm-dir>/hull-checkpoint`, next to `disk.img`. The
disk is copied with an APFS clone, which cannot cross volumes, and hull's store
is a volume of its own. The instance's `checkpoint` directory links to it. The
clone costs no space at first, and grows as the guest's disk changes. `hull rm`
leaves it in place, and the next `hull run` of the directory clears it.

## Recording the window

`--gui-record <file.mov>` records the guest's window from the first frame.
vz-runner captures its own window, so no Screen Recording permission is needed.
`kill -USR2 <runner pid>` ends the recording and leaves the guest running.

## The guest agent

`hull exec` talks to `hull-guest-agent`, a launchd daemon inside the guest. It
is a port of urunc's urunit-agent and speaks the same protocol on the same
vsock port. Build it and install it in a running guest over SSH:

```bash
GOOS=darwin GOARCH=arm64 go build -o build/hull-guest-agent ./cmd/hull-guest-agent
scripts/macos-guest-provision.sh build/hull-guest-agent admin@<guest-ip>
```

A session runs as the user logged in at the guest's console, inside that user's
desktop session, so GUI programs work: `hull exec <name> -- open -a Safari`.
`--user root` runs as root.

`hull stop` asks the agent to shut the system down. A guest with no agent gets
a stop request it most probably ignores, and is forced off after the grace
period.

## One application in the window

`packaging/macos-guest/claude-desktop/` holds an example that opens an
application at login and puts it into full screen, so the window on the host
shows the application alone. It needs a guest where `osascript` is allowed to
use Accessibility, as in the Cirrus Labs images.

## Not there yet

- The guest image has to carry the agent. hull does not build images.
- `hull pull` does not know Tart's OCI format. Use `tart clone`.
- Extra block disks and the network gateway are not wired up.
- macOS allows two macOS guests per host.
