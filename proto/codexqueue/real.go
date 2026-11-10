package codexqueue

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// FlockHeld reports whether someone holds the flock on path: a non-blocking exclusive
// attempt failing with EAGAIN. A missing file is an error, never "free".
func FlockHeld(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return false, nil
	}
	if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) {
		return true, nil
	}
	return false, err
}

// LsofOpeners lists the pids with path open, excluding this process. ponytail: shells out
// to lsof; the product would need a raw proc_info call to stay cgo-free and shell-free.
func LsofOpeners(path string) ([]int, error) {
	out, err := exec.Command("lsof", "-F", "p", "--", path).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(out) == 0 {
			return nil, nil
		}
		return nil, err
	}
	var pids []int
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.HasPrefix(line, "p") {
			continue
		}
		if n, err := strconv.Atoi(line[1:]); err == nil && n != os.Getpid() {
			pids = append(pids, n)
		}
	}
	return pids, nil
}
