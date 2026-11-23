package opencode

import (
	"bufio"
	"context"
	"encoding/hex"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func osPeerPIDs(ctx context.Context, self, svc netip.AddrPort) ([]int, error) {
	inode := ""
	for _, table := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		found, err := connInode(table, svc, self)
		if err != nil {
			return nil, err
		}
		if found != "" {
			inode = found
		}
	}
	if inode == "" || inode == "0" {
		return nil, nil
	}
	procs, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	target := "socket:[" + inode + "]"
	var pids []int
	for _, p := range procs {
		pid, err := strconv.Atoi(p.Name())
		if err != nil {
			continue
		}
		dir := filepath.Join("/proc", p.Name(), "fd")
		fds, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if link, err := os.Readlink(filepath.Join(dir, fd.Name())); err == nil && link == target {
				pids = append(pids, pid)
				break
			}
		}
	}
	return pids, nil
}

// connInode returns the inode of the socket in table whose local end is local
// and remote end is remote.
func connInode(table string, local, remote netip.AddrPort) (string, error) {
	f, err := os.Open(table)
	if errors.Is(err, os.ErrNotExist) && strings.HasSuffix(table, "6") {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	s.Scan()
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) < 10 {
			continue
		}
		l, ok1 := procAddr(fields[1])
		r, ok2 := procAddr(fields[2])
		if ok1 && ok2 && l == local && r == remote {
			return fields[9], nil
		}
	}
	return "", s.Err()
}

// procAddr parses a /proc/net/tcp address: the IP as 32-bit words in host
// (little-endian) order, a colon, then the port in big-endian hex.
func procAddr(field string) (netip.AddrPort, bool) {
	ipHex, portHex, ok := strings.Cut(field, ":")
	raw, err := hex.DecodeString(ipHex)
	port, perr := strconv.ParseUint(portHex, 16, 16)
	if !ok || err != nil || perr != nil || (len(raw) != 4 && len(raw) != 16) {
		return netip.AddrPort{}, false
	}
	for i := 0; i < len(raw); i += 4 {
		raw[i], raw[i+1], raw[i+2], raw[i+3] = raw[i+3], raw[i+2], raw[i+1], raw[i]
	}
	addr, _ := netip.AddrFromSlice(raw)
	return netip.AddrPortFrom(addr.Unmap(), uint16(port)), true
}
