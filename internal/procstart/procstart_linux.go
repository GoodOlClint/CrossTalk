package procstart

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// /proc/<pid>/stat starttime is in USER_HZ ticks since boot; USER_HZ is 100 on
// every Linux ABI Go supports.
const userHZ = 100

var (
	bootOnce sync.Once
	bootTime time.Time
	bootErr  error
)

func boot() (time.Time, error) {
	bootOnce.Do(func() {
		b, err := os.ReadFile("/proc/stat")
		if err != nil {
			bootErr = err
			return
		}
		for line := range strings.SplitSeq(string(b), "\n") {
			if v, ok := strings.CutPrefix(line, "btime "); ok {
				sec, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
				if err != nil {
					bootErr = err
					return
				}
				bootTime = time.Unix(sec, 0).UTC()
				return
			}
		}
		bootErr = errors.New("procstart: btime not found in /proc/stat")
	})
	return bootTime, bootErr
}

// startTicks is /proc/<pid>/stat starttime: ticks since boot, exact for one
// process however the wall clock moves.
func startTicks(pid int) (int64, error) {
	if pid <= 0 {
		return 0, ErrNoProcess
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, ErrNoProcess
		}
		return 0, err
	}
	// comm (field 2) may contain spaces and parentheses; fields resume after the last ')'.
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return 0, fmt.Errorf("procstart: malformed /proc/%d/stat", pid)
	}
	f := strings.Fields(string(b[i+1:]))
	const starttimeIdx = 22 - 3
	if len(f) <= starttimeIdx {
		return 0, fmt.Errorf("procstart: short /proc/%d/stat", pid)
	}
	return strconv.ParseInt(f[starttimeIdx], 10, 64)
}

// Token is a string that is equal across two readings of one process and
// differs for another process with the pid, also after a reboot.
func Token(pid int) (string, error) {
	t, err := startTicks(pid)
	if err != nil {
		return "", err
	}
	id, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	return token(strings.TrimSpace(string(id)), t), nil
}

func token(bootID string, ticks int64) string {
	return bootID + ":" + strconv.FormatInt(ticks, 10)
}

func StartTime(pid int) (time.Time, error) {
	ticks, err := startTicks(pid)
	if err != nil {
		return time.Time{}, err
	}
	bt, err := boot()
	if err != nil {
		return time.Time{}, err
	}
	return bt.Add(time.Duration(ticks) * (time.Second / userHZ)), nil
}
