package ownerproof

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func failListFDs(t *testing.T, pid int, errno unix.Errno) {
	t.Helper()
	prev := procInfo
	procInfo = func(call, p, flavor int, arg uint64, buf []byte) (int, error) {
		if p == pid && call == procInfoCallPIDInfo {
			return 0, errno
		}
		return prev(call, p, flavor, arg, buf)
	}
	t.Cleanup(func() { procInfo = prev })
}

func TestOpeners_ProcessDeniesInspection_ExpectError(t *testing.T) {
	path := lockFile(t)
	h := startHelper(t, "read", lockFile(t))
	failListFDs(t, h.pid, unix.EPERM)
	if got, err := OpenersAmong(path, []int{h.pid}); !errors.Is(err, unix.EPERM) {
		t.Fatalf("openers %v err %v, want EPERM", got, err)
	}
}

func TestOpeners_ProcessGoneMidWalk_ExpectSkipped(t *testing.T) {
	path := lockFile(t)
	reader := startHelper(t, "read", path)
	gone := startHelper(t, "read", path)
	failListFDs(t, gone.pid, unix.ESRCH)
	got, err := OpenersAmong(path, []int{reader.pid, gone.pid})
	if err != nil || !slices.Equal(got, []int{reader.pid}) {
		t.Fatalf("openers %v err %v, want only %d", got, err, reader.pid)
	}
}

func failFDInfo(t *testing.T, pid int, errno unix.Errno) {
	t.Helper()
	prev := procInfo
	procInfo = func(call, p, flavor int, arg uint64, buf []byte) (int, error) {
		if p == pid && call == procInfoCallPIDFDInfo {
			return 0, errno
		}
		return prev(call, p, flavor, arg, buf)
	}
	t.Cleanup(func() { procInfo = prev })
}

func TestOpeners_FDInfoFailsWithIOError_ExpectError(t *testing.T) {
	path := lockFile(t)
	h := startHelper(t, "read", lockFile(t))
	failFDInfo(t, h.pid, unix.EIO)
	if got, err := OpenersAmong(path, []int{h.pid}); !errors.Is(err, unix.EIO) {
		t.Fatalf("openers %v err %v, want EIO", got, err)
	}
}

func TestOpeners_FDInfoDeniedEPERM_ExpectFDSkippedNoError(t *testing.T) {
	path := lockFile(t)
	h := startHelper(t, "read", lockFile(t))
	failFDInfo(t, h.pid, unix.EPERM)
	if _, err := OpenersAmong(path, []int{h.pid}); err != nil {
		t.Fatalf("err %v: a per-fd EPERM must not fail every proof", err)
	}
}

func nonDumpable() error { return errors.ErrUnsupported }

func TestOpeners_FDInfoENOENTRevokedFD_ExpectFDSkippedNoError(t *testing.T) {
	path := lockFile(t)
	h := startHelper(t, "read", lockFile(t))
	failFDInfo(t, h.pid, unix.ENOENT)
	if got, err := OpenersAmong(path, []int{h.pid}); err != nil || len(got) != 0 {
		t.Fatalf("openers %v err %v: a revoked fd in an unrelated process must not fail the proof", got, err)
	}
}

func TestOpeners_RealOpenerBesideRevokedFD_ExpectOpenerStillFound(t *testing.T) {
	path := lockFile(t)
	revoked := startHelper(t, "read", lockFile(t))
	opener := startHelper(t, "read", path)
	failFDInfo(t, revoked.pid, unix.ENOENT)
	got, err := OpenersAmong(path, []int{revoked.pid, opener.pid})
	if err != nil || !slices.Equal(got, []int{opener.pid}) {
		t.Fatalf("openers %v err %v, want only %d", got, err, opener.pid)
	}
}

func failFDNumber(t *testing.T, pid int, fd uint64, errno unix.Errno) {
	t.Helper()
	prev := procInfo
	procInfo = func(call, p, flavor int, arg uint64, buf []byte) (int, error) {
		if p == pid && call == procInfoCallPIDFDInfo && arg == fd {
			return 0, errno
		}
		return prev(call, p, flavor, arg, buf)
	}
	t.Cleanup(func() { procInfo = prev })
}

func TestOpeners_RevokedFDThenLockFDInSameProcess_ExpectProcessStillCounted(t *testing.T) {
	path := lockFile(t)
	unrelated, err := os.Open(lockFile(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unrelated.Close() })
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), helperMode+"=read", helperPath+"="+path)
	cmd.ExtraFiles = []*os.File{unrelated}
	stdin, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stdin.Close(); cmd.Process.Kill(); cmd.Wait() })
	if line, err := bufio.NewReader(out).ReadString('\n'); err != nil || !strings.HasPrefix(line, "ready") {
		t.Fatalf("helper did not start: %q %v", line, err)
	}
	failFDNumber(t, cmd.Process.Pid, 3, unix.ENOENT)
	got, err := OpenersAmong(path, []int{cmd.Process.Pid})
	if err != nil || !slices.Equal(got, []int{cmd.Process.Pid}) {
		t.Fatalf("openers %v err %v, want the process: an unreadable earlier fd must not hide a later fd on the lock", got, err)
	}
}

func TestOpeners_ProcessFDListENOENT_ExpectError(t *testing.T) {
	path := lockFile(t)
	h := startHelper(t, "read", lockFile(t))
	failListFDs(t, h.pid, unix.ENOENT)
	if got, err := OpenersAmong(path, []int{h.pid}); !errors.Is(err, unix.ENOENT) {
		t.Fatalf("openers %v err %v, want ENOENT: a process whose fds cannot be listed must fail closed", got, err)
	}
}

func TestOpeners_UnreadableFDBesideOnlyOpener_ExpectUnreadableProcessCounted(t *testing.T) {
	path := lockFile(t)
	parent := startHelper(t, "read", path)
	hidden := startHelper(t, "read", lockFile(t))
	failFDInfo(t, hidden.pid, unix.EPERM)
	confirmed, unreadable, err := scanAmong(path, []int{parent.pid, hidden.pid})
	if err != nil || !slices.Equal(confirmed, []int{parent.pid}) || !slices.Equal(unreadable, []int{hidden.pid}) {
		t.Fatalf("confirmed %v unreadable %v err %v, want %d and %d: a process with an fd the kernel will not describe may hold the lock", confirmed, unreadable, err, parent.pid, hidden.pid)
	}
}

func TestOpeners_UnreadableFDOnSystemBinary_ExpectNotCounted(t *testing.T) {
	for _, c := range []struct {
		exe  string
		want bool
	}{
		{"/usr/libexec/routined", true},
		{"/System/Library/Frameworks/X.framework/progressd", true},
		{"/usr/local/bin/evil", false},
		{"/Users/u/bin/evil", false},
		{"/usr/libexec/../../tmp/evil", false},
		{"", false},
	} {
		if got := systemBinary(c.exe); got != c.want {
			t.Errorf("systemBinary(%q) = %v, want %v", c.exe, got, c.want)
		}
	}
}

func TestOpeners_UnreadableFDOnSystemProcess_ExpectSkippedButConfirmedKept(t *testing.T) {
	path := lockFile(t)
	sysProc := startHelper(t, "read", lockFile(t))
	holder := startHelper(t, "read", path)
	failFDInfo(t, sysProc.pid, unix.EPERM)
	failFDNumber(t, holder.pid, 3, unix.EPERM)
	prev := processExe
	processExe = func(int) string { return "/usr/libexec/routined" }
	t.Cleanup(func() { processExe = prev })
	confirmed, unreadable, err := scanAmong(path, []int{sysProc.pid, holder.pid})
	if err != nil || len(unreadable) != 0 || len(confirmed) > 1 {
		t.Fatalf("confirmed %v unreadable %v err %v: a system binary's unreadable fd is skipped", confirmed, unreadable, err)
	}
	processExe = func(int) string { return "/Users/u/evil" }
	_, unreadable, err = scanAmong(path, []int{sysProc.pid})
	if err != nil || !slices.Equal(unreadable, []int{sysProc.pid}) {
		t.Fatalf("unreadable %v err %v, want %d", unreadable, err, sysProc.pid)
	}
}
