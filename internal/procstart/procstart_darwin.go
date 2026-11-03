package procstart

import (
	"errors"
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

func StartTime(pid int) (time.Time, error) {
	if pid <= 0 {
		return time.Time{}, ErrNoProcess
	}
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		if errors.Is(err, unix.ESRCH) || errors.Is(err, unix.ENOENT) || errors.Is(err, unix.EIO) {
			return time.Time{}, ErrNoProcess
		}
		return time.Time{}, fmt.Errorf("procstart: sysctl kern.proc.pid.%d: %w", pid, err)
	}
	if int(kp.Proc.P_pid) != pid {
		return time.Time{}, ErrNoProcess
	}
	tv := kp.Proc.P_starttime
	return time.Unix(int64(tv.Sec), int64(tv.Usec)*1000).UTC(), nil
}
