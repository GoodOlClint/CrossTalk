#!/usr/bin/env python3
"""Run a command in a pty for a CI leg: log its output, answer terminal queries, accept keystrokes from a fifo.

usage: ptyrun.py LOG FIFO CMD [ARG...]
Answers the cursor-position query, and answers the Codex hooks-review dialog with "2" + Enter ("Trust all and continue") once,
only when $HOME is a disposable directory under a temp dir (never the user's real home).
"""
import fcntl, os, pty, select, signal, struct, sys, tempfile, termios, time

TRIGGERS = [(b"Hooks need review", b"2\r")]


def disposable_home():
    home = os.path.realpath(os.environ.get("HOME", ""))
    roots = {os.path.realpath(r) for r in (tempfile.gettempdir(), "/tmp", os.environ.get("RUNNER_TEMP", "/nonexistent"))}
    return any(home.startswith(r + os.sep) for r in roots)


def main():
    log_path, fifo, cmd = sys.argv[1], sys.argv[2], sys.argv[3:]
    pid, fd = pty.fork()
    if pid == 0:
        os.execvp(cmd[0], cmd)
    fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", 40, 120, 0, 0))
    inp = os.open(fifo, os.O_RDWR | os.O_NONBLOCK)
    log = open(log_path, "wb", buffering=0)
    fired = set()
    if not disposable_home():
        fired = set(range(len(TRIGGERS)))
        log.write(b"ptyrun: HOME is not a disposable directory, so no dialog is answered\n")
    window = b""
    signal.signal(signal.SIGTERM, lambda *_: (os.kill(pid, signal.SIGKILL), sys.exit(0)))
    while True:
        r, _, _ = select.select([fd, inp], [], [], 0.5)
        if fd in r:
            try:
                d = os.read(fd, 65536)
            except OSError:
                break
            if not d:
                break
            log.write(d)
            window = (window + d)[-4096:]
            if b"\x1b[6n" in d:
                os.write(fd, b"\x1b[1;1R")
            for i, (text, reply) in enumerate(TRIGGERS):
                if i not in fired and text in window:
                    fired.add(i)
                    time.sleep(0.5)
                    os.write(fd, reply)
        if inp in r:
            try:
                k = os.read(inp, 4096)
            except BlockingIOError:
                k = b""
            if k:
                os.write(fd, k)
        done, _ = os.waitpid(pid, os.WNOHANG)
        if done:
            break


if __name__ == "__main__":
    main()
