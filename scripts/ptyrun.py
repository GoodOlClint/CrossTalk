#!/usr/bin/env python3
"""Run a command in a pty for a CI leg: log its output, answer terminal queries, accept keystrokes from a fifo.

usage: ptyrun.py LOG FIFO CMD [ARG...]   |   ptyrun.py --match CAPTURED_BYTES_FILE
Answers the cursor-position query, and answers the Codex hooks-review dialog with "2" + Enter ("Trust all and continue") once,
only when PTYRUN_ANSWER_HOOKS=1 (set by the queue phase alone) and $HOME is a scratch directory under /tmp (neither RUNNER_TEMP nor TMPDIR is trusted) and both $CODEX_HOME (which must be set) and $HOME/.codex resolve, symlinks followed, inside it.
"""
import codecs, fcntl, os, pty, re, select, signal, struct, sys, termios, time

# Matched against the rendered screen, not the byte stream: Codex's TUI redraws only the cells that changed, so
# "Hooks need review" is never contiguous in the bytes (it writes "Hooks ", moves the cursor, then "eed review").
TRIGGERS = [("Hooks need review", b"2\r")]


class Screen:
    """A tiny terminal screen: enough cursor addressing and erasing to read back what a full-screen TUI shows."""

    def __init__(self, rows=40, cols=120):
        self.rows, self.cols = rows, cols
        self.grid = [[" "] * cols for _ in range(rows)]
        self.r = self.c = 0
        self.dec = codecs.getincrementaldecoder("utf-8")(errors="replace")
        self.pending = ""

    def put(self, ch):
        if 0 <= self.r < self.rows and 0 <= self.c < self.cols:
            self.grid[self.r][self.c] = ch
        self.c = min(self.c + 1, self.cols - 1)

    def csi(self, params, final):
        nums = [int(x) if x.isdigit() else 0 for x in re.split(r"[;:]", params.lstrip("?<>=!"))] if params else []
        n = lambda i, d=1: nums[i] if len(nums) > i and nums[i] else d
        if params and params[0] in "?<>=!":
            return
        if final in "Hf":
            self.r, self.c = min(n(0) - 1, self.rows - 1), min(n(1) - 1, self.cols - 1)
        elif final == "A":
            self.r = max(self.r - n(0), 0)
        elif final == "B":
            self.r = min(self.r + n(0), self.rows - 1)
        elif final == "C":
            self.c = min(self.c + n(0), self.cols - 1)
        elif final == "D":
            self.c = max(self.c - n(0), 0)
        elif final == "G":
            self.c = min(n(0) - 1, self.cols - 1)
        elif final == "d":
            self.r = min(n(0) - 1, self.rows - 1)
        elif final == "K":
            mode = nums[0] if nums else 0
            lo, hi = {0: (self.c, self.cols), 1: (0, self.c + 1), 2: (0, self.cols)}.get(mode, (0, 0))
            for x in range(lo, hi):
                self.grid[self.r][x] = " "
        elif final == "X":
            for x in range(self.c, min(self.c + n(0), self.cols)):
                self.grid[self.r][x] = " "
        elif final == "J":
            mode = nums[0] if nums else 0
            rows = range(self.r + 1, self.rows) if mode == 0 else range(0, self.r) if mode == 1 else range(self.rows)
            for y in rows:
                self.grid[y] = [" "] * self.cols
            if mode in (0, 2):
                for x in range(self.c if mode == 0 else 0, self.cols):
                    self.grid[self.r][x] = " "

    def feed(self, data):
        text = self.pending + self.dec.decode(data)
        self.pending = ""
        i = 0
        while i < len(text):
            ch = text[i]
            if ch == "\x1b":
                if i + 1 >= len(text):
                    self.pending = text[i:]
                    return
                nxt = text[i + 1]
                if nxt == "[":
                    m = re.compile(r"[0-?]*[ -/]*[@-~]").match(text, i + 2)
                    if not m:
                        self.pending = text[i:]
                        return
                    seq = m.group(0)
                    self.csi(seq[:-1].rstrip(" -/"), seq[-1])
                    i = m.end()
                elif nxt == "]":
                    m = re.compile(r"[^\x07\x1b]*(\x07|\x1b\\)").match(text, i + 2)
                    if not m:
                        self.pending = text[i:]
                        return
                    i = m.end()
                else:
                    i += 2
            elif ch == "\r":
                self.c = 0
                i += 1
            elif ch == "\n":
                self.r = min(self.r + 1, self.rows - 1)
                i += 1
            elif ch == "\b":
                self.c = max(self.c - 1, 0)
                i += 1
            elif ch >= " ":
                self.put(ch)
                i += 1
            else:
                i += 1

    def text(self):
        return "\n".join("".join(row).rstrip() for row in self.grid)


def inside(path, root):
    return path.startswith(root + os.sep)


def matched(screen_text):
    flat = re.sub(r"\s+", " ", screen_text)
    return [t for t, _ in TRIGGERS if t in flat]


def disposable_home():
    """True only for a scratch HOME under /tmp, with CODEX_HOME and $HOME/.codex both inside it after symlinks are resolved."""
    if os.environ.get("PTYRUN_ANSWER_HOOKS") != "1":
        return False
    raw = os.environ.get("HOME", "")
    if not raw:
        return False
    home = os.path.realpath(raw)
    if not inside(home, os.path.realpath("/tmp")):
        return False
    codex_home = os.environ.get("CODEX_HOME", "")
    if not codex_home:
        return False
    return inside(os.path.realpath(codex_home), home) and inside(os.path.realpath(os.path.join(home, ".codex")), home)


def main():
    if len(sys.argv) == 3 and sys.argv[1] == "--match":
        screen = Screen()
        screen.feed(open(sys.argv[2], "rb").read())
        found = matched(screen.text())
        print("\n".join(found))
        sys.exit(0 if len(found) == len(TRIGGERS) else 1)
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
        log.write(b"ptyrun: PTYRUN_ANSWER_HOOKS is not 1 or HOME is not a disposable directory, so no dialog is answered\n")
    screen = Screen()
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
            screen.feed(d)
            if b"\x1b[6n" in d:
                os.write(fd, b"\x1b[1;1R")
            flat = re.sub(r"\s+", " ", screen.text())
            for i, (text, reply) in enumerate(TRIGGERS):
                if i not in fired and text in flat:
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
