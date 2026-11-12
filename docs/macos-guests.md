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
- Shared directories, secrets and the network gateway are not wired up.
- Checkpoint and restore are passed through to the runner, but not tested with
  a macOS guest.
- macOS allows two macOS guests per host.
