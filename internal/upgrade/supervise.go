package upgrade

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// SuperviseCmd is the hidden crosstalk subcommand Command starts.
const SuperviseCmd = "upgrade-exec"

const (
	superviseLimit = 5 * time.Minute
	superviseDrain = 10 * time.Second
)

type lockKey struct{}

// Command is exec.CommandContext, except inside a follow-up target: there it
// starts `crosstalk upgrade-exec`, which holds upgrade.lock on fd 3 and runs
// name in its own process group, killing that group at ctx's deadline (5
// minutes without one) or on SIGTERM, which cancelling ctx sends. The
// supervisor keeps its hold on the lock until it exits, even if the shim is
// killed; name and its descendants never get it.
func Command(ctx context.Context, name string, args ...string) *exec.Cmd {
	f, _ := ctx.Value(lockKey{}).(*os.File)
	exe, err := os.Executable()
	if f == nil || err != nil {
		return exec.CommandContext(ctx, name, args...)
	}
	limit := superviseLimit
	if d, ok := ctx.Deadline(); ok {
		limit = time.Until(d)
	}
	c := exec.CommandContext(ctx, exe, append([]string{SuperviseCmd, limit.String(), "--", name}, args...)...)
	c.ExtraFiles = []*os.File{f}
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error { return c.Process.Signal(syscall.SIGTERM) }
	c.WaitDelay = superviseDrain
	return c
}

// Supervise is `crosstalk upgrade-exec <limit> -- name args...`.
func Supervise(args []string, stderr io.Writer) int {
	if len(args) < 3 || args[1] != "--" {
		fmt.Fprintln(stderr, "usage: crosstalk "+SuperviseCmd+" <limit> -- command [args...]")
		return 2
	}
	limit, err := time.ParseDuration(args[0])
	if err != nil {
		fmt.Fprintf(stderr, "crosstalk %s: %v\n", SuperviseCmd, err)
		return 2
	}
	unix.CloseOnExec(3)
	// Caught, not ignored, so the command starts with default dispositions;
	// SIGPIPE comes from writing to a killed shim's stderr.
	signal.Notify(make(chan os.Signal, 1), syscall.SIGHUP, syscall.SIGINT, syscall.SIGPIPE)
	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGTERM)
	c := exec.Command(args[2], args[3:]...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, bestEffort{os.Stdout}, bestEffort{os.Stderr}
	c.WaitDelay = superviseDrain
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := c.Start(); err != nil {
		fmt.Fprintln(stderr, err)
		return 127
	}
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	select {
	case err := <-done:
		var ee *exec.ExitError
		switch {
		case err == nil:
			return 0
		case errors.As(err, &ee) && ee.ExitCode() > 0:
			return ee.ExitCode()
		default:
			return 1
		}
	case <-time.After(limit):
		syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
		fmt.Fprintf(stderr, "%s: killed after %s\n", args[2], limit)
	case <-term:
		syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
		fmt.Fprintf(stderr, "%s: cancelled by SIGTERM\n", args[2])
	}
	<-done
	return 1
}

// bestEffort forwards to w and drops what w refuses, so the command never
// writes to a pipe whose reader, the shim, may be gone.
type bestEffort struct{ w io.Writer }

func (b bestEffort) Write(p []byte) (int, error) {
	b.w.Write(p)
	return len(p), nil
}
