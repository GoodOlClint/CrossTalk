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
// A process that exits or denies inspection mid-walk is skipped.
func Openers(path string) ([]int, error) { return openers(path) }
