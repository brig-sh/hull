# Emulates an interactive shell's job control: session leader with a ctty
# spawns hull in its own process group, puts it in the foreground,
# and waits with WUNTRACED — exactly how zsh runs a job. A SIGTTOU-suspended
# job is reported instead of silently hanging.
import os, pty, sys, time, fcntl, termios, select, signal

BIN = sys.argv[1]; hv = sys.argv[2]; name = sys.argv[3]
STORE = os.environ.get("HULL_STORE_DIR", "")
GLOBAL_ARGS = (["--store-dir", STORE] if STORE else [])
IMG = "harbor.nbfc.io/nubificus/urunc-ubuntu-vz:aarch64"

master, slave = pty.openpty()
shell = os.fork()
if shell == 0:
    os.setsid()
    fcntl.ioctl(slave, termios.TIOCSCTTY, 0)
    os.dup2(slave, 0); os.dup2(slave, 1); os.dup2(slave, 2)
    if slave > 2: os.close(slave)
    os.close(master)
    child = os.fork()
    if child == 0:
        os.setpgid(0, 0)
        os.execv(BIN, [BIN] + GLOBAL_ARGS + ["run", "--hypervisor", hv, "--net", "none",
                       "--stop-grace", "3", "--name", name, "--", IMG])
    os.setpgid(child, child)
    os.tcsetpgrp(0, child)          # job to foreground, like zsh
    _, status = os.waitpid(child, os.WUNTRACED)
    if os.WIFSTOPPED(status):
        sig = os.WSTOPSIG(status)
        os.write(2, f"FAKESHELL: job SUSPENDED by signal {sig} (SIGTTOU bug)\n".encode())
        os.kill(child, signal.SIGKILL); os.waitpid(child, 0)
        os._exit(3)
    os.write(2, b"FAKESHELL: job ended cleanly\n")
    os._exit(0)

os.close(slave)
def drain(sec):
    out=b""; end=time.time()+sec
    while time.time()<end:
        r,_,_=select.select([master],[],[],0.4)
        if r:
            try: out+=os.read(master,4096)
            except OSError: break
    return out
# 'Run /.' is the init-wrapper exec line hull's own boot writes to the
# console before handing off to the guest command (see pty-terminal-test.py,
# which waits on the same marker, same env var and same 180s default); it is
# common to both the vz and qemu rootfs modes. Its presence is the only
# positive evidence that hull, and the guest under it, actually ran -- not
# just that the fake shell wrapper around it forked and exited. Without it
# there is nothing to call CLEAN: a missing or instantly-crashing hull
# binary produces empty console output and an ordinary (non-stopped) exit
# from the fake shell, which used to be indistinguishable from a real clean
# run.
#
# Wait for the marker (or for the fake shell exiting on its own -- a
# legitimate CLEAN outcome if the guest halts before we get here) instead of
# a fixed short sleep: a real but slow qemu boot on a loaded runner must not
# be misread as a boot that never happened.
BOOT_MARKER = b"Run /."
BOOT_TIMEOUT = int(os.environ.get("HULL_TEST_BOOT_TIMEOUT", "180"))
console = b""
st = 0
shell_reaped = False
deadline = time.time() + BOOT_TIMEOUT
while time.time() < deadline:
    console += drain(1)
    if BOOT_MARKER in console:
        break
    reaped, st = os.waitpid(shell, os.WNOHANG)
    if reaped == shell:
        shell_reaped = True
        console += drain(1)  # flush whatever the guest wrote in the same
                             # window as the shell exiting, so a marker
                             # written right before exit still counts
        break

if not shell_reaped:
    console += drain(2)  # settle
    # ^C at the guest prompt. If the job already ended (guest halted before
    # we got here), the fake shell has exited and closed the pty slave, so
    # this write raises EIO. That is not a failure: fall through to the
    # shell's exit status, which is the actual verdict (CLEAN vs
    # SUSPENDED-BUG).
    try:
        os.write(master, b"\x03")
    except OSError:
        pass
    console += drain(12)
    _, st = os.waitpid(shell, 0)

tail = console[-160:]
print(f"console tail: {tail!r}")
booted = BOOT_MARKER in console

if os.WEXITSTATUS(st) == 3:
    verdict = 'SUSPENDED-BUG'
elif booted:
    verdict = 'CLEAN'
else:
    verdict = 'NO-BOOT-EVIDENCE'
print(f"verdict: {verdict}")
sys.exit(0 if verdict == 'CLEAN' else 1)
