// Package ownerproof reads the kernel facts an owner proof is built from:
// a process's parent and start time, its ancestry, whether a flock is held,
// and which processes have a file open. It is cgo-free and supports darwin
// and linux; elsewhere every read returns ErrUnsupported.
package ownerproof

import (
	"errors"
	"time"
)

var (
	ErrUnsupported = errors.New("ownerproof: unsupported platform")
	ErrNoProcess   = errors.New("ownerproof: no such process")
	ErrTooDeep     = errors.New("ownerproof: ancestry deeper than the walk bound")
	ErrPIDReused   = errors.New("ownerproof: a parent started after its child (pid reused)")
)

// maxDepth bounds an ancestry walk; a real chain is a few dozen at most.
const maxDepth = 256

// Proc is one process as the kernel reports it; (PID, Start) names it across
// pid reuse.
type Proc struct {
	PID   int
	PPID  int
	Start time.Time
}

func (p Proc) Same(o Proc) bool { return p.PID == o.PID && p.Start.Equal(o.Start) }

func Info(pid int) (Proc, error) {
	if pid <= 0 {
		return Proc{}, ErrNoProcess
	}
	return info(pid)
}

// Args returns the executable path and argv of pid.
func Args(pid int) (exe string, argv []string, err error) {
	if pid <= 0 {
		return "", nil, ErrNoProcess
	}
	return args(pid)
}

// Ancestry walks from pid up through each parent, pid first, ending at a
// process whose parent is 0 (pid 1 or a container's init). Every step is a
// fresh kernel read; an exited link, or a parent that started after its
// child, is an error.
func Ancestry(pid int) ([]Proc, error) { return AncestryWith(Info, pid) }

// AncestryWith is Ancestry over another source of process facts.
func AncestryWith(info func(int) (Proc, error), pid int) ([]Proc, error) {
	var chain []Proc
	for len(chain) < maxDepth {
		p, err := info(pid)
		if err != nil {
			return nil, err
		}
		if n := len(chain); n > 0 && p.Start.After(chain[n-1].Start) {
			return nil, ErrPIDReused
		}
		chain = append(chain, p)
		if p.PPID <= 0 || p.PID == 1 {
			return chain, nil
		}
		pid = p.PPID
	}
	return nil, ErrTooDeep
}

// LockHeld reports whether some process holds a flock on path: a
// non-blocking exclusive attempt fails with EWOULDBLOCK. A missing or
// unreadable file is an error, never "free".
func LockHeld(path string) (bool, error) { return lockHeld(path) }

// Openers returns the pids of processes this user can inspect that have the
// file at path open, matched by device and inode, excluding this process.
// A process that exits mid-walk is skipped; one whose fds cannot be listed is
// an error. On macOS a single fd that fails with EBADF (closed mid-walk) or
// ENOENT (revoked) is skipped; one that fails with EPERM (denied) makes the
// process a counted opener unless its executable is an Apple binary under
// /System (not /System/Volumes), /usr (not /usr/local), /bin or /sbin.
func Openers(path string) ([]int, error) {
	pids, err := userPIDs()
	if err != nil {
		return nil, err
	}
	return OpenersAmong(path, pids)
}

// ExecutableIs reports whether pid's mapped executable is the file now at
// path (same device and inode). A binary replaced at path while pid runs is
// false.
func ExecutableIs(pid int, path string) (bool, error) {
	if pid <= 0 {
		return false, ErrNoProcess
	}
	return executableIs(pid, path)
}

// OpenersAmong is Openers over the given pids only, by the same rules. Its
// result is the confirmed openers followed by the unreadable ones.
func OpenersAmong(path string, pids []int) ([]int, error) {
	confirmed, unreadable, err := scanAmong(path, pids)
	if err != nil {
		return nil, err
	}
	return append(confirmed, unreadable...), nil
}

// OpenersSplit is Openers with the two kinds kept apart: confirmed processes
// matched by device and inode, and unreadable ones that could not be ruled out
// (macOS only: an EPERM fd, unless the process is an Apple system binary). An
// unreadable process is never proof that a file is open.
func OpenersSplit(path string) (confirmed, unreadable []int, err error) {
	pids, err := userPIDs()
	if err != nil {
		return nil, nil, err
	}
	return scanAmong(path, pids)
}

// OpenersAmongSplit is OpenersSplit over the given pids only.
func OpenersAmongSplit(path string, pids []int) (confirmed, unreadable []int, err error) {
	return scanAmong(path, pids)
}
