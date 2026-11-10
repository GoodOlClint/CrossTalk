package ownerproof

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"

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

func openers(path string) ([]int, error) {
	target, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	self := os.Getpid()
	var out []int
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		fdDir := "/proc/" + e.Name() + "/fd"
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			fi, err := os.Stat(fdDir + "/" + fd.Name())
			if err == nil && os.SameFile(fi, target) {
				out = append(out, pid)
				break
			}
		}
	}
	return out, nil
}
