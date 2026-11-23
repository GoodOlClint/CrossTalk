package main

import (
	"cmp"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/upgrade"
)

func shRunner(st, script, pidFile string, timeout time.Duration) *upgrade.Runner {
	return &upgrade.Runner{StateDir: st, Version: "9.9.9", TargetTimeout: timeout, Targets: []upgrade.Target{{Name: "codex",
		Run: func(ctx context.Context, _ io.Writer) error {
			_, err := installRun(ctx, "sh", "-c", script, pidFile)
			return err
		},
		Verify: func(context.Context) error { return nil }}}}
}

func TestFollowUpChildHelper(t *testing.T) {
	st, pidFile := os.Getenv("CROSSTALK_TEST_FOLLOWUP_STATE"), os.Getenv("CROSSTALK_TEST_FOLLOWUP_PIDFILE")
	if st == "" {
		t.Skip("helper process only")
	}
	timeout, _ := time.ParseDuration(os.Getenv("CROSSTALK_TEST_FOLLOWUP_TIMEOUT"))
	os.Setenv("CROSSTALK_TEST_MAIN", "1")
	script := cmp.Or(os.Getenv("CROSSTALK_TEST_FOLLOWUP_SCRIPT"), `echo $$ > "$0"; exec sleep 30`)
	shRunner(st, script, pidFile, timeout).Run(context.Background())
}

func waitPID(t *testing.T, pidFile string) int {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); ; {
		if b, err := os.ReadFile(pidFile); err == nil {
			if pid, _ := strconv.Atoi(strings.TrimSpace(string(b))); pid > 0 {
				t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the child never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func lockFree(st string) bool {
	rel, ok, _ := upgrade.TryLock(st)
	if ok {
		rel()
	}
	return ok
}

func gone(pid int) bool { return syscall.Kill(pid, 0) == syscall.ESRCH }

func within(d time.Duration, cond func() bool) bool {
	for deadline := time.Now().Add(d); !cond(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			return false
		}
	}
	return true
}

func killedShim(t *testing.T, timeout time.Duration, env ...string) (st string, child int, pidFile string) {
	t.Helper()
	st = t.TempDir()
	pidFile = filepath.Join(t.TempDir(), "child.pid")
	shim := exec.Command(os.Args[0], "-test.run=^TestFollowUpChildHelper$")
	shim.Env = append(os.Environ(), "CROSSTALK_TEST_FOLLOWUP_STATE="+st, "CROSSTALK_TEST_FOLLOWUP_PIDFILE="+pidFile,
		"CROSSTALK_TEST_FOLLOWUP_TIMEOUT="+timeout.String())
	shim.Env = append(shim.Env, env...)
	if err := shim.Start(); err != nil {
		t.Fatal(err)
	}
	child = waitPID(t, pidFile)
	shim.Process.Kill()
	shim.Wait()
	return st, child, pidFile
}

func TestFollowUpChildKeepsLockAfterShimKilled(t *testing.T) {
	st, child, _ := killedShim(t, time.Minute)
	if pg, err := syscall.Getpgid(child); err != nil || pg != child {
		t.Errorf("child pgid = %d (%v), want its own group %d", pg, err, child)
	}
	if lockFree(st) {
		t.Fatal("upgrade.lock free while the killed shim's child still runs")
	}
	syscall.Kill(child, syscall.SIGKILL)
	if !within(5*time.Second, func() bool { return lockFree(st) }) {
		t.Fatal("upgrade.lock still held after the child exited")
	}
}

func TestFollowUpChildDeadlineOutlivesShim(t *testing.T) {
	st, child, _ := killedShim(t, time.Second)
	if !within(10*time.Second, func() bool { return gone(child) && lockFree(st) }) {
		t.Fatalf("after the shim was killed, the target deadline did not end the child (gone %v) and free the lock (%v)", gone(child), lockFree(st))
	}
}

func TestFollowUpChildDescendantDoesNotHoldLock(t *testing.T) {
	t.Setenv("CROSSTALK_TEST_MAIN", "1")
	st := t.TempDir()
	pidFile := filepath.Join(t.TempDir(), "bg.pid")
	shRunner(st, `sleep 30 </dev/null >/dev/null 2>&1 & echo $! > "$0"`, pidFile, time.Minute).Run(context.Background())
	bg := waitPID(t, pidFile)
	if gone(bg) {
		t.Fatal("the background process is already gone; the test proves nothing")
	}
	if !upgrade.Done(st, "9.9.9", "codex") || !lockFree(st) {
		t.Fatalf("a process the step left running holds upgrade.lock (done %v)", upgrade.Done(st, "9.9.9", "codex"))
	}
}

func TestFollowUpChildCancelKillsGroup(t *testing.T) {
	t.Setenv("CROSSTALK_TEST_MAIN", "1")
	st := t.TempDir()
	pidFile := filepath.Join(t.TempDir(), "bg.pid")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		shRunner(st, `sleep 30 & echo $! > "$0"; wait`, pidFile, time.Minute).Run(ctx)
		close(done)
	}()
	bg := waitPID(t, pidFile)
	cancel()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after cancel while a grandchild held the output pipe")
	}
	if !within(5*time.Second, func() bool { return gone(bg) }) {
		t.Fatal("the grandchild outlived the cancelled step")
	}
	if !lockFree(st) {
		t.Fatal("upgrade.lock held after the cancelled step")
	}
	b, _ := os.ReadFile(upgrade.LogPath(st, "9.9.9"))
	if log := string(b); !strings.Contains(log, "codex: cancelled:") || !strings.Contains(log, "sh: cancelled by SIGTERM") || strings.Contains(log, "failed") {
		t.Fatalf("log does not name the cancel:\n%s", b)
	}
}

func TestFollowUpChildWritesAfterShimKilled(t *testing.T) {
	st, child, pidFile := killedShim(t, time.Minute, "CROSSTALK_TEST_FOLLOWUP_SCRIPT="+
		`echo $$ > "$0"; sleep 1; i=0; while [ $i -lt 2000 ]; do echo out line $i; echo err line $i >&2; i=$((i+1)); done; echo ok > "$0.done"`)
	if !within(15*time.Second, func() bool { return gone(child) }) {
		t.Fatal("the child never finished")
	}
	if _, err := os.Stat(pidFile + ".done"); err != nil {
		t.Fatal("a child writing output after the shim was killed did not finish its step")
	}
	if !within(15*time.Second, func() bool { return lockFree(st) }) {
		t.Fatal("upgrade.lock held after the child finished")
	}
}

func TestFollowUpChildErrorKeepsStderr(t *testing.T) {
	t.Setenv("CROSSTALK_TEST_MAIN", "1")
	st := t.TempDir()
	shRunner(st, `echo boom-from-codex >&2; exit 3`, "", time.Minute).Run(context.Background())
	b, _ := os.ReadFile(upgrade.LogPath(st, "9.9.9"))
	if !strings.Contains(string(b), "boom-from-codex") || !strings.Contains(string(b), "exit status 3") {
		t.Fatalf("log lacks the command's stderr and exit status:\n%s", b)
	}
}

func TestFollowUpChildOrphanReapedByNextHolder(t *testing.T) {
	st, child, _ := killedShim(t, 2*time.Second)
	out, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(child)).Output()
	sup, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || sup <= 1 {
		t.Fatalf("no supervisor for child %d: %q %v", child, out, err)
	}
	syscall.Kill(sup, syscall.SIGKILL)
	if !within(5*time.Second, func() bool { return gone(sup) }) || gone(child) {
		t.Fatal("setup: the supervisor must die and leave the child running")
	}
	rel, ok, err := upgrade.TryLock(st)
	if !ok {
		t.Fatalf("lock not free after the supervisor died: %v", err)
	}
	defer rel()
	if !within(3*time.Second, func() bool { return gone(child) }) {
		t.Fatal("the orphaned command still runs after the next holder took the lock")
	}
}
