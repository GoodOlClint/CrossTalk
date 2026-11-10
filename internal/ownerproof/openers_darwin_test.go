package ownerproof

import (
	"errors"
	"slices"
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
