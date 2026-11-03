package procstart

import (
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestStartTimeSelf(t *testing.T) {
	st, err := StartTime(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if st.After(now) || now.Sub(st) > time.Hour {
		t.Fatalf("start time %v implausible (now %v)", st, now)
	}
	again, _ := StartTime(os.Getpid())
	if !again.Equal(st) {
		t.Fatalf("start time not stable: %v vs %v", st, again)
	}
}

func TestStartTimeChildDiffersAndDies(t *testing.T) {
	self, _ := StartTime(os.Getpid())
	time.Sleep(20 * time.Millisecond)
	cmd := exec.Command("sleep", "5")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	st, err := StartTime(pid)
	if err != nil {
		t.Fatal(err)
	}
	if !st.After(self) {
		t.Fatalf("child start %v not after parent %v", st, self)
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if _, err := StartTime(pid); !errors.Is(err, ErrNoProcess) {
		t.Fatalf("reaped pid: err = %v", err)
	}
}

func TestStartTimeInvalidPID(t *testing.T) {
	for _, pid := range []int{0, -1, 1 << 30} {
		if _, err := StartTime(pid); !errors.Is(err, ErrNoProcess) {
			t.Errorf("pid %d: err = %v", pid, err)
		}
	}
}
