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
	reapOrphan(ctx, f.Name()+".child", nil)
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
		writeChild(m, c.Process.Pid, time.Now().Add(limit), limit)
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

// writeChild records "pgid deadline-unix-nano start-token limit-nanoseconds"
// for reapOrphan, through a rename so a kill never leaves a partial record.
// It writes nothing when the leader's start token is unknown.
func writeChild(path string, pgid int, deadline time.Time, limit time.Duration) {
	st := startTime(pgid)
	if st == "" {
		return
	}
	tmp := path + ".tmp"
	if os.WriteFile(tmp, []byte(fmt.Sprintf("%d %d %s %d\n", pgid, deadline.UnixNano(), st, int64(limit))), 0o600) == nil {
		os.Rename(tmp, path)
	}
}

// startTime is pid's start token (procstart.Token), "" when unknown.
func startTime(pid int) string {
	t, err := procstart.Token(pid)
	if err != nil {
		return ""
	}
	return t
}

func groupAlive(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// reapOrphan runs under upgrade.lock. A record left by a supervisor that was
// killed names a command group that may still run with no deadline: wait for
// it until the recorded deadline, at most the recorded limit from now so a
// backward wall-clock step cannot lengthen the wait, then SIGKILL the group; a
// cancelled ctx leaves the record and the group. A record of the earlier
// three-field format is dropped without signalling its group. The group is signalled
// only while its leader still has the recorded start time, or is gone with
// members left (the kernel keeps a pid unused while a group has it); a leader
// with another start time means the id was reused, and a record with no start
// time is never acted on. The record stays when the signal fails.
func reapOrphan(ctx context.Context, path string, waiting func()) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	f := strings.Fields(string(b))
	if len(f) != 4 {
		os.Remove(path)
		return
	}
	pg, err1 := strconv.ParseInt(f[0], 10, 32)
	ns, err2 := strconv.ParseInt(f[1], 10, 64)
	lim, err3 := strconv.ParseInt(f[3], 10, 64)
	pgid := int(pg)
	if err1 != nil || err2 != nil || err3 != nil || lim < 0 || pgid <= 1 {
		os.Remove(path)
		return
	}
	ours := func() bool {
		cur := startTime(pgid)
		return cur == "" || cur == f[2]
	}
	deadline := time.Unix(0, ns)
	if capped := time.Now().Add(time.Duration(lim)); capped.Before(deadline) {
		deadline = capped
	}
	for told := false; groupAlive(pgid) && ours() && time.Now().Before(deadline); {
		if !told && waiting != nil {
			waiting()
			told = true
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
	if groupAlive(pgid) && ours() {
		if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return
		}
	}
	os.Remove(path)
}
