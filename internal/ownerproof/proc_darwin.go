package ownerproof

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

func info(pid int) (Proc, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		if errors.Is(err, unix.ESRCH) || errors.Is(err, unix.ENOENT) || errors.Is(err, unix.EIO) {
			return Proc{}, ErrNoProcess
		}
		return Proc{}, fmt.Errorf("ownerproof: sysctl kern.proc.pid.%d: %w", pid, err)
	}
	if int(kp.Proc.P_pid) != pid {
		return Proc{}, ErrNoProcess
	}
	tv := kp.Proc.P_starttime
	return Proc{PID: pid, PPID: int(kp.Eproc.Ppid), Start: time.Unix(int64(tv.Sec), int64(tv.Usec)*1000).UTC()}, nil
}

// kern.procargs2 layout: int32 argc, the exec path, NUL padding, then argc
// NUL-terminated arguments, then the environment.
func args(pid int) (string, []string, error) {
	b, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		if errors.Is(err, unix.ESRCH) {
			return "", nil, ErrNoProcess
		}
		return "", nil, fmt.Errorf("ownerproof: sysctl kern.procargs2.%d: %w", pid, err)
	}
	if len(b) < 4 {
		return "", nil, fmt.Errorf("ownerproof: short procargs2 for %d", pid)
	}
	argc := int(binary.NativeEndian.Uint32(b))
	b = b[4:]
	i := bytes.IndexByte(b, 0)
	if i < 0 {
		return "", nil, fmt.Errorf("ownerproof: malformed procargs2 for %d", pid)
	}
	exe := string(b[:i])
	b = bytes.TrimLeft(b[i:], "\x00")
	argv := make([]string, 0, argc)
	for len(argv) < argc && len(b) > 0 {
		j := bytes.IndexByte(b, 0)
		if j < 0 {
			j = len(b)
		}
		argv = append(argv, string(b[:j]))
		b = b[min(j+1, len(b)):]
	}
	return exe, argv, nil
}

// <sys/proc_info.h>. proc_fdinfo is {int32 fd; uint32 type}. vnode_fdinfo is
// proc_fileinfo (24 bytes) then vnode_info, whose vinfo_stat starts with
// uint32 vst_dev and has uint64 vst_ino at offset 8.
const (
	procInfoCallPIDInfo   = 2
	procInfoCallPIDFDInfo = 3
	procPIDListFDs        = 1
	procPIDFDVnodeInfo    = 1
	proxFDTypeVnode       = 1
	procFDInfoSize        = 8
	vnodeFDInfoSize       = 176
	vstDevOff             = 24
	vstInoOff             = 24 + 8
)

func procInfo(call, pid, flavor int, arg uint64, buf []byte) (int, error) {
	var p unsafe.Pointer
	if len(buf) > 0 {
		p = unsafe.Pointer(&buf[0])
	}
	r, _, e := unix.Syscall6(unix.SYS_PROC_INFO, uintptr(call), uintptr(pid), uintptr(flavor), uintptr(arg), uintptr(p), uintptr(len(buf)))
	if e != 0 {
		return 0, e
	}
	return int(r), nil
}

func listFDs(pid int) ([]byte, error) {
	buf := make([]byte, 256*procFDInfoSize)
	for {
		n, err := procInfo(procInfoCallPIDInfo, pid, procPIDListFDs, 0, buf)
		if err != nil {
			return nil, err
		}
		if n < len(buf) {
			return buf[:n], nil
		}
		buf = make([]byte, 2*len(buf))
	}
}

func openers(path string) ([]int, error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return nil, &os.PathError{Op: "stat", Path: path, Err: err}
	}
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.uid", os.Geteuid())
	if err != nil {
		return nil, fmt.Errorf("ownerproof: list processes: %w", err)
	}
	self := os.Getpid()
	var out []int
	vbuf := make([]byte, vnodeFDInfoSize)
	for _, kp := range procs {
		pid := int(kp.Proc.P_pid)
		if pid == self || pid <= 0 {
			continue
		}
		fds, err := listFDs(pid)
		if err != nil {
			continue
		}
		for i := 0; i+procFDInfoSize <= len(fds); i += procFDInfoSize {
			if binary.NativeEndian.Uint32(fds[i+4:]) != proxFDTypeVnode {
				continue
			}
			fd := int32(binary.NativeEndian.Uint32(fds[i:]))
			n, err := procInfo(procInfoCallPIDFDInfo, pid, procPIDFDVnodeInfo, uint64(fd), vbuf)
			if err != nil || n < vnodeFDInfoSize {
				continue
			}
			if binary.NativeEndian.Uint32(vbuf[vstDevOff:]) == uint32(st.Dev) && binary.NativeEndian.Uint64(vbuf[vstInoOff:]) == st.Ino {
				out = append(out, pid)
				break
			}
		}
	}
	return out, nil
}
