package upgrade

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/GoodOlClint/crosstalk/internal/procstart"
)

// SuperviseCmd is the hidden crosstalk subcommand Command starts.
const SuperviseCmd = "upgrade-exec"

const (
	superviseLimit = 5 * time.Minute
	superviseDrain = 10 * time.Second
)

// childEnv names the file the supervisor records its command's process group
// in; ChildPath derives it from the lock path.
const childEnv = "CROSSTALK_UPGRADE_CHILD"

type lockKey struct{}

func ChildPath(stateDir string) string { return LockPath(stateDir) + ".child" }

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
	c.Env = append(os.Environ(), childEnv+"="+f.Name()+".child")
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
	if m := os.Getenv(childEnv); m != "" {
		writeChild(m, c.Process.Pid, time.Now().Add(limit))
		defer os.Remove(m)
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

// writeChild records "pgid deadline-unix-nano start-time" for reapOrphan.
func writeChild(path string, pgid int, deadline time.Time) {
	os.WriteFile(path, []byte(fmt.Sprintf("%d %d %s\n", pgid, deadline.UnixNano(), startTime(pgid))), 0o600)
}

// startTime is pid's start time in unix nanoseconds, "" when unknown.
func startTime(pid int) string {
	t, err := procstart.StartTime(pid)
	if err != nil {
		return ""
	}
	return strconv.FormatInt(t.UnixNano(), 10)
}

func groupAlive(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// reapOrphan runs under upgrade.lock. A marker left by a supervisor that was
// killed names a command group that may still run with no deadline: wait for
// it until the recorded deadline, then SIGKILL the group. A pgid whose leader
// now has another start time was reused, and is left alone.
func reapOrphan(stateDir string) {
	path := ChildPath(stateDir)
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	defer os.Remove(path)
	f := strings.SplitN(strings.TrimSpace(string(b)), " ", 3)
	if len(f) < 2 {
		return
	}
	pgid, err1 := strconv.Atoi(f[0])
	ns, err2 := strconv.ParseInt(f[1], 10, 64)
	if err1 != nil || err2 != nil || pgid <= 1 {
		return
	}
	for deadline := time.Unix(0, ns); groupAlive(pgid) && time.Now().Before(deadline); {
		time.Sleep(50 * time.Millisecond)
	}
	if !groupAlive(pgid) {
		return
	}
	if cur := startTime(pgid); cur != "" && len(f) == 3 && cur != f[2] {
		return
	}
	syscall.Kill(-pgid, syscall.SIGKILL)
}
