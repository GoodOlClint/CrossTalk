package opencode

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const tcpListen = "0A"

func osListenerPIDs(ctx context.Context, port int) ([]int, error) {
	inodes := map[string]bool{}
	for _, table := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		if err := listenInodes(table, port, inodes); err != nil {
			return nil, err
		}
	}
	if len(inodes) == 0 {
		return nil, nil
	}
	procs, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	held := map[string]bool{}
	var pids []int
	for _, p := range procs {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		pid, err := strconv.Atoi(p.Name())
		if err != nil {
			continue
		}
		dir := filepath.Join("/proc", p.Name(), "fd")
		fds, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		holds := false
		for _, fd := range fds {
			link, err := os.Readlink(filepath.Join(dir, fd.Name()))
			if err != nil || !strings.HasPrefix(link, "socket:[") {
				continue
			}
			inode := strings.TrimSuffix(strings.TrimPrefix(link, "socket:["), "]")
			if inodes[inode] {
				held[inode] = true
				holds = true
			}
		}
		if holds {
			pids = append(pids, pid)
		}
	}
	for inode := range inodes {
		if !held[inode] {
			return nil, fmt.Errorf("listening socket inode %s has no visible holder", inode)
		}
	}
	return pids, nil
}

func listenInodes(table string, port int, into map[string]bool) error {
	f, err := os.Open(table)
	if errors.Is(err, os.ErrNotExist) && strings.HasSuffix(table, "6") {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	want := fmt.Sprintf(":%04X", port)
	s := bufio.NewScanner(f)
	s.Scan()
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) < 10 || fields[3] != tcpListen || !strings.HasSuffix(fields[1], want) {
			continue
		}
		into[fields[9]] = true
	}
	return s.Err()
}
