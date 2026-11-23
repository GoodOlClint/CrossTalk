package opencode

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strconv"
)

func osListenerPIDs(ctx context.Context, port int) ([]int, error) {
	out, err := exec.CommandContext(ctx, "/usr/sbin/lsof", "-nP", "-b", "-w",
		"-iTCP:"+strconv.Itoa(port), "-sTCP:LISTEN", "-Fp").Output()
	var exit *exec.ExitError
	if err != nil && !(errors.As(err, &exit) && exit.ExitCode() == 1 && len(out) == 0) {
		return nil, err
	}
	var pids []int
	for _, line := range bytes.Split(out, []byte("\n")) {
		if len(line) > 1 && line[0] == 'p' {
			pid, err := strconv.Atoi(string(line[1:]))
			if err != nil {
				return nil, err
			}
			pids = append(pids, pid)
		}
	}
	return pids, nil
}
