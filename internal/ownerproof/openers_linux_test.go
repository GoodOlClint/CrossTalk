package ownerproof

import (
	"errors"
	"io/fs"
	"os"
	"slices"
	"strconv"
	"syscall"
	"testing"
)

func failFDDir(t *testing.T, pid int, err error) {
	t.Helper()
	prev := readFDDir
	readFDDir = func(dir string) ([]os.DirEntry, error) {
		if dir == "/proc/"+strconv.Itoa(pid)+"/fd" {
			return nil, err
		}
		return prev(dir)
	}
	t.Cleanup(func() { readFDDir = prev })
}

func TestOpeners_ProcessDeniesInspection_ExpectError(t *testing.T) {
	path := lockFile(t)
	h := startHelper(t, "read", lockFile(t))
	failFDDir(t, h.pid, &fs.PathError{Op: "open", Path: "fd", Err: syscall.EACCES})
	if got, err := Openers(path); !errors.Is(err, syscall.EACCES) {
		t.Fatalf("openers %v err %v, want EACCES", got, err)
	}
}

func TestOpeners_ProcessGoneMidWalk_ExpectSkipped(t *testing.T) {
	path := lockFile(t)
	reader := startHelper(t, "read", path)
	gone := startHelper(t, "read", path)
	failFDDir(t, gone.pid, &fs.PathError{Op: "open", Path: "fd", Err: syscall.ENOENT})
	got, err := Openers(path)
	if err != nil || !slices.Equal(got, []int{reader.pid}) {
		t.Fatalf("openers %v err %v, want only %d", got, err, reader.pid)
	}
}
