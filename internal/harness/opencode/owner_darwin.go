package opencode

import (
	"bytes"
	"context"
	"errors"
	"net/netip"
	"os/exec"
	"slices"
	"strconv"
)

func osPeerPIDs(ctx context.Context, self, svc netip.AddrPort) ([]int, error) {
	out, err := exec.CommandContext(ctx, "/usr/sbin/lsof", "-nP", "-b", "-w", "-iTCP@"+self.String(), "-Fpn").Output()
	var exit *exec.ExitError
	if err != nil && !(errors.As(err, &exit) && exit.ExitCode() == 1 && len(out) == 0) {
		return nil, err
	}
	want := "n" + svc.String() + "->" + self.String()
	var pids []int
	pid := 0
	for _, line := range bytes.Split(out, []byte("\n")) {
		switch {
		case len(line) > 1 && line[0] == 'p':
			if pid, err = strconv.Atoi(string(line[1:])); err != nil {
				return nil, err
			}
		case string(line) == want && pid > 0 && !slices.Contains(pids, pid):
			pids = append(pids, pid)
		}
	}
	return pids, nil
}
