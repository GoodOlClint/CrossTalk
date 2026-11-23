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

const (
	tcpEstablished = "01"
	tcpSynRecv     = "03"
	tcpListen      = "0A"
)

// osPeerPIDs holds back an unaccepted connection (a service that sets
// TCP_DEFER_ACCEPT accepts an idle one only after about a second) to the
// holders of the listening sockets it would be accepted from.
func osPeerPIDs(ctx context.Context, self, svc netip.AddrPort) ([]int, error) {
	var inodes []string
	pending := false
	for _, table := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		found, state, err := connInode(table, svc, self)
		if err != nil {
			return nil, err
		}
		switch {
		case found == "":
		case found == "0" && (state == tcpEstablished || state == tcpSynRecv):
			pending = true
		case found != "0":
			inodes = append(inodes, found)
		}
	}
	if len(inodes) == 0 && pending {
		for _, table := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
			ls, err := listenInodes(table, svc)
			if err != nil {
				return nil, err
			}
			inodes = append(inodes, ls...)
		}
	}
	if len(inodes) == 0 {
		return nil, nil
	}
	procs, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	targets := make(map[string]bool, len(inodes))
	for _, in := range inodes {
		targets["socket:["+in+"]"] = true
	}
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
			if link, err := os.Readlink(filepath.Join(dir, fd.Name())); err == nil && targets[link] {
				pids = append(pids, pid)
				break
			}
		}
	}
	return pids, nil
}

// connInode returns the inode and TCP state of the socket in table whose
// local end is local and remote end is remote.
func connInode(table string, local, remote netip.AddrPort) (inode, state string, err error) {
	rows, err := tcpRows(table)
	for _, f := range rows {
		l, ok1 := procAddr(f[1])
		r, ok2 := procAddr(f[2])
		if ok1 && ok2 && l == local && r == remote {
			return f[9], f[3], nil
		}
	}
	return "", "", err
}

// listenInodes returns the inodes of the listening sockets in table that
// accept connections to svc: bound to its address or to the unspecified one.
func listenInodes(table string, svc netip.AddrPort) ([]string, error) {
	rows, err := tcpRows(table)
	var out []string
	for _, f := range rows {
		l, ok := procAddr(f[1])
		if ok && f[3] == tcpListen && f[9] != "0" && l.Port() == svc.Port() && (l.Addr() == svc.Addr() || l.Addr().IsUnspecified()) {
			out = append(out, f[9])
		}
	}
	return out, err
}

func tcpRows(table string) ([][]string, error) {
	f, err := os.Open(table)
	if errors.Is(err, os.ErrNotExist) && strings.HasSuffix(table, "6") {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var rows [][]string
	s := bufio.NewScanner(f)
	s.Scan()
	for s.Scan() {
		if fields := strings.Fields(s.Text()); len(fields) >= 10 {
			rows = append(rows, fields)
		}
	}
	return rows, s.Err()
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
