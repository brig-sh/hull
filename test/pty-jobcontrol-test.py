# Emulates an interactive shell's job control: session leader with a ctty
# spawns urunc-macos in its own process group, puts it in the foreground,
# and waits with WUNTRACED — exactly how zsh runs a job. A SIGTTOU-suspended
# job is reported instead of silently hanging.
import os, pty, sys, time, fcntl, termios, select, signal

BIN = sys.argv[1]; hv = sys.argv[2]; name = sys.argv[3]
STORE = os.environ.get("URUNC_STORE_DIR", "")
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
drain(9)
# ^C at the guest prompt. If the job already ended (guest halted before we
# got here — a legitimate CLEAN outcome, common under the slower QEMU boot
# on a loaded runner), the fake shell has exited and closed the pty slave,
# so this write raises EIO. That is not a failure: fall through to the
# shell's exit status, which is the actual verdict (CLEAN vs SUSPENDED-BUG).
try:
    os.write(master, b"\x03")
except OSError:
    pass
out = drain(12)
_, st = os.waitpid(shell, 0)
tail = out[-160:]
print(f"console tail: {tail!r}")
verdict = 'SUSPENDED-BUG' if os.WEXITSTATUS(st) == 3 else 'CLEAN'
print(f"verdict: {verdict}")
sys.exit(0 if verdict == 'CLEAN' else 1)
