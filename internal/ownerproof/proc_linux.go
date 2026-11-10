package ownerproof

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/GoodOlClint/crosstalk/internal/procstart"
)

func info(pid int) (Proc, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Proc{}, ErrNoProcess
		}
		return Proc{}, err
	}
	// comm (field 2) may hold spaces and parentheses; state and ppid follow the last ')'.
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return Proc{}, fmt.Errorf("ownerproof: malformed /proc/%d/stat", pid)
	}
	f := strings.Fields(string(b[i+1:]))
	if len(f) < 2 {
		return Proc{}, fmt.Errorf("ownerproof: short /proc/%d/stat", pid)
	}
	ppid, err := strconv.Atoi(f[1])
	if err != nil {
		return Proc{}, err
	}
	st, err := procstart.StartTime(pid)
	if errors.Is(err, procstart.ErrNoProcess) {
		return Proc{}, ErrNoProcess
	}
	if err != nil {
		return Proc{}, err
	}
	return Proc{PID: pid, PPID: ppid, Start: st}, nil
}

func args(pid int) (string, []string, error) {
	dir := "/proc/" + strconv.Itoa(pid)
	b, err := os.ReadFile(dir + "/cmdline")
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil, ErrNoProcess
		}
		return "", nil, err
	}
	argv := strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")
	exe, err := os.Readlink(dir + "/exe")
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil, ErrNoProcess
		}
		return "", nil, err
	}
	return exe, argv, nil
}

func userPIDs() ([]int, error) {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, e := range ents {
		if pid, err := strconv.Atoi(e.Name()); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

func openersAmong(path string, pids []int) ([]int, error) {
	target, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	self, uid := os.Getpid(), os.Geteuid()
	var out []int
	for _, pid := range pids {
		if pid == self || pid <= 0 {
			continue
		}
		dir := "/proc/" + strconv.Itoa(pid)
		mine, err := ownedBy(dir+"/status", uid)
		if gone(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !mine {
			continue
		}
		found, err := opensFile(dir+"/fd", target)
		if gone(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("ownerproof: open files of %d: %w", pid, err)
		}
		if found {
			out = append(out, pid)
		}
	}
	return out, nil
}

// ownedBy reads the Uid line of /proc/<pid>/status (real, effective, saved,
// filesystem): a non-dumpable process of this user has a root-owned /proc
// entry, so the directory owner is not the test.
func ownedBy(status string, uid int) (bool, error) {
	b, err := os.ReadFile(status)
	if err != nil {
		return false, err
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "Uid:"); ok {
			for _, f := range strings.Fields(v) {
				if f == strconv.Itoa(uid) {
					return true, nil
				}
			}
			return false, nil
		}
	}
	return false, fmt.Errorf("ownerproof: no Uid line in %s", status)
}

var readFDDir = os.ReadDir

func gone(err error) bool { return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) }

func opensFile(fdDir string, target os.FileInfo) (bool, error) {
	fds, err := readFDDir(fdDir)
	if err != nil {
		return false, err
	}
	for _, fd := range fds {
		fi, err := os.Stat(fdDir + "/" + fd.Name())
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		if os.SameFile(fi, target) {
			return true, nil
		}
	}
	return false, nil
}

func executableIs(pid int, path string) (bool, error) {
	target, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	fi, err := os.Stat("/proc/" + strconv.Itoa(pid) + "/exe")
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, ErrNoProcess
		}
		return false, err
	}
	return os.SameFile(fi, target), nil
}
