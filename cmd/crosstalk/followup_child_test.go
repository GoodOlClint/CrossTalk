package main

import (
	"context"
	"errors"
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

func TestFollowUpChildHelper(t *testing.T) {
	st, pidFile := os.Getenv("CROSSTALK_TEST_FOLLOWUP_STATE"), os.Getenv("CROSSTALK_TEST_FOLLOWUP_PIDFILE")
	if st == "" {
		t.Skip("helper process only")
	}
	r := &upgrade.Runner{StateDir: st, Version: "9.9.9", Targets: []upgrade.Target{{Name: "codex",
		Run: func(ctx context.Context, _ io.Writer) error {
			_, err := installRun(ctx, "sh", "-c", `echo $$ > "$0"; exec sleep 30`, pidFile)
			return err
		},
		Verify: func(context.Context) error { return nil }}}}
	r.Run(context.Background())
}

func TestFollowUpChildKeepsLockAfterShimKilled(t *testing.T) {
	st := t.TempDir()
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	shim := exec.Command(os.Args[0], "-test.run=^TestFollowUpChildHelper$")
	shim.Env = append(os.Environ(), "CROSSTALK_TEST_FOLLOWUP_STATE="+st, "CROSSTALK_TEST_FOLLOWUP_PIDFILE="+pidFile)
	if err := shim.Start(); err != nil {
		t.Fatal(err)
	}
	var child int
	for deadline := time.Now().Add(20 * time.Second); child == 0; {
		if b, err := os.ReadFile(pidFile); err == nil {
			child, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
		if time.Now().After(deadline) {
			shim.Process.Kill()
			t.Fatal("the helper shim never started its child")
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Cleanup(func() { syscall.Kill(child, syscall.SIGKILL) })
	if pg, err := syscall.Getpgid(child); err != nil || pg != child {
		t.Errorf("child pgid = %d (%v), want its own group %d", pg, err, child)
	}
	shim.Process.Kill()
	shim.Wait()
	if rel, ok, err := upgrade.TryLock(st); err != nil || ok {
		if ok {
			rel()
		}
		t.Fatalf("upgrade.lock free while the killed shim's child still runs (err %v)", err)
	}
	syscall.Kill(child, syscall.SIGKILL)
	for deadline := time.Now().Add(5 * time.Second); ; {
		rel, ok, err := upgrade.TryLock(st)
		if ok {
			rel()
			break
		}
		if err != nil || time.Now().After(deadline) {
			t.Fatalf("upgrade.lock still held after the child exited: %v", errors.Join(err))
		}
		time.Sleep(20 * time.Millisecond)
	}
}
