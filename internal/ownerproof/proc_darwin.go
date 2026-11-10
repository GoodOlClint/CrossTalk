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
	// proc_regionwithpathinfo: proc_regioninfo (96 bytes, pri_address at 80,
	// pri_size at 88) then vnode_info_path, whose vinfo_stat starts at 96.
	procPIDRegionPathInfo = 8
	regionPathInfoSize    = 1272
	regionAddrOff         = 80
	regionSizeOff         = 88
	regionDevOff          = 96
	regionInoOff          = 96 + 8
	maxRegions            = 1 << 16
)

var procInfo = rawProcInfo

func rawProcInfo(call, pid, flavor int, arg uint64, buf []byte) (int, error) {
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

func userPIDs() ([]int, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.uid", os.Geteuid())
	if err != nil {
		return nil, fmt.Errorf("ownerproof: list processes: %w", err)
	}
	pids := make([]int, 0, len(procs))
	for _, kp := range procs {
		pids = append(pids, int(kp.Proc.P_pid))
	}
	return pids, nil
}

func openersAmong(path string, pids []int) ([]int, error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return nil, &os.PathError{Op: "stat", Path: path, Err: err}
	}
	self := os.Getpid()
	var out []int
	vbuf := make([]byte, vnodeFDInfoSize)
	for _, pid := range pids {
		if pid == self || pid <= 0 {
			continue
		}
		found, err := opensFile(pid, &st, vbuf)
		if errors.Is(err, unix.ESRCH) {
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

// opensFile fails with ESRCH when pid is gone; any other failure means pid
// could not be inspected.
func opensFile(pid int, st *unix.Stat_t, vbuf []byte) (bool, error) {
	fds, err := listFDs(pid)
	if err != nil {
		return false, err
	}
	for i := 0; i+procFDInfoSize <= len(fds); i += procFDInfoSize {
		if binary.NativeEndian.Uint32(fds[i+4:]) != proxFDTypeVnode {
			continue
		}
		fd := int32(binary.NativeEndian.Uint32(fds[i:]))
		n, err := procInfo(procInfoCallPIDFDInfo, pid, procPIDFDVnodeInfo, uint64(fd), vbuf)
		// ponytail: macOS answers EPERM for some fds of same-user system agents
		// (measured: 1 to 34 vnode fds each in 8 agents on one host), so failing
		// closed here would fail every proof. Such an fd is skipped: an opener
		// whose only fd on the lock is one the kernel will not describe is not
		// seen. Whole-process denial still fails closed above.
		if errors.Is(err, unix.EBADF) || errors.Is(err, unix.EPERM) {
			continue
		}
		if err != nil {
			return false, err
		}
		if n < vnodeFDInfoSize {
			return false, fmt.Errorf("short vnode info for fd %d", fd)
		}
		if binary.NativeEndian.Uint32(vbuf[vstDevOff:]) == uint32(st.Dev) && binary.NativeEndian.Uint64(vbuf[vstInoOff:]) == st.Ino {
			return true, nil
		}
	}
	return false, nil
}

// executableIs walks pid's memory regions for one mapped from the file at
// path; the walk ends with EINVAL past the last region.
func executableIs(pid int, path string) (bool, error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return false, &os.PathError{Op: "stat", Path: path, Err: err}
	}
	buf := make([]byte, regionPathInfoSize)
	var addr uint64
	for range maxRegions {
		n, err := procInfo(procInfoCallPIDInfo, pid, procPIDRegionPathInfo, addr, buf)
		if errors.Is(err, unix.ESRCH) {
			return false, ErrNoProcess
		}
		if errors.Is(err, unix.EINVAL) || err == nil && n == 0 {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("ownerproof: regions of %d: %w", pid, err)
		}
		if n < regionPathInfoSize {
			return false, fmt.Errorf("ownerproof: short region info for %d", pid)
		}
		if binary.NativeEndian.Uint32(buf[regionDevOff:]) == uint32(st.Dev) && binary.NativeEndian.Uint64(buf[regionInoOff:]) == st.Ino {
			return true, nil
		}
		next := binary.NativeEndian.Uint64(buf[regionAddrOff:]) + binary.NativeEndian.Uint64(buf[regionSizeOff:])
		if next <= addr {
			return false, fmt.Errorf("ownerproof: region walk of %d did not advance", pid)
		}
		addr = next
	}
	return false, fmt.Errorf("ownerproof: more than %d regions in %d", maxRegions, pid)
}
