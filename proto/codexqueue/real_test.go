package codexqueue

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Real-kernel checks for the owner-proof inference primitives on the host running the
// tests: a non-blocking flock attempt and the lsof opener list. The holder is a child
// process (this test binary re-executed), standing in for the codex process.

func TestMain(m *testing.M) {
	if mode := os.Getenv("CQ_HELPER_MODE"); mode != "" {
		path := os.Getenv("CQ_HELPER_PATH")
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			os.Exit(2)
		}
		if mode == "hold" {
			if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
				os.Exit(3)
			}
		}
		os.Stdout.WriteString("ready\n")
		bufio.NewReader(os.Stdin).ReadString('\n')
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type helper struct {
	cmd *exec.Cmd
	pid int
}

func startHelper(t *testing.T, mode, path string) *helper {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "CQ_HELPER_MODE="+mode, "CQ_HELPER_PATH="+path)
	stdin, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "ready") {
		t.Fatalf("helper %s did not start: %q %v", mode, line, err)
	}
	h := &helper{cmd: cmd, pid: cmd.Process.Pid}
	t.Cleanup(func() { stdin.Close(); cmd.Process.Kill(); cmd.Wait() })
	return h
}

func lockFile(t *testing.T) (dir, path string) {
	t.Helper()
	dir = t.TempDir()
	path = filepath.Join(dir, "T1.lock")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, path
}

func needLsof(t *testing.T) {
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("lsof not available")
	}
}

type realSys struct {
	*fakeSys
	dir string
}

func (r realSys) LockHeld(thread string) (bool, error) {
	return FlockHeld(filepath.Join(r.dir, thread+".lock"))
}
func (r realSys) Openers(thread string) ([]int, error) {
	return LsofOpeners(filepath.Join(r.dir, thread+".lock"))
}

func TestReal_FlockHeldByChildThenKilled9_ExpectHeldThenReleased(t *testing.T) {
	_, path := lockFile(t)
	if held, err := FlockHeld(path); err != nil || held {
		t.Fatalf("free file reported held=%v err=%v", held, err)
	}
	h := startHelper(t, "hold", path)
	if held, err := FlockHeld(path); err != nil || !held {
		t.Fatalf("held file reported held=%v err=%v", held, err)
	}
	h.cmd.Process.Signal(syscall.SIGKILL)
	h.cmd.Wait()
	deadline := time.Now().Add(2 * time.Second)
	for {
		held, _ := FlockHeld(path)
		if !held {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("lock still held after kill -9")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestReal_FlockOnMissingFile_ExpectError(t *testing.T) {
	if _, err := FlockHeld(filepath.Join(t.TempDir(), "nope.lock")); err == nil {
		t.Fatal("missing lock file must be an error, not 'free'")
	}
}

func TestReal_OwnerProofHolderIsParentOnlyOpener_ExpectBound(t *testing.T) {
	needLsof(t)
	dir, path := lockFile(t)
	h := startHelper(t, "hold", path)
	f := newFake()
	f.parent = Proc{PID: h.pid, Start: 1, Args: []string{"codex", "--no-daemon"}}
	s := NewShim(realSys{f, dir})
	if err := s.AcceptDurable(Claim{SessionID: "T1"}); err != nil {
		t.Fatal(err)
	}
}

func TestReal_OwnerProofWithReaderProcessOpener_ExpectSecondOpenerFailsClosed(t *testing.T) {
	needLsof(t)
	dir, path := lockFile(t)
	h := startHelper(t, "hold", path)
	startHelper(t, "read", path)
	f := newFake()
	f.parent = Proc{PID: h.pid, Start: 1, Args: []string{"codex", "--no-daemon"}}
	s := NewShim(realSys{f, dir})
	if err := s.AcceptDurable(Claim{SessionID: "T1"}); !errors.Is(err, ErrSecondOpener) {
		t.Fatalf("%v", err)
	}
}

func TestReal_OwnerProofParentOnlyReadsLockHeldByOther_ExpectSecondOpenerNotAccepted(t *testing.T) {
	needLsof(t)
	dir, path := lockFile(t)
	startHelper(t, "hold", path)
	reader := startHelper(t, "read", path)
	f := newFake()
	f.parent = Proc{PID: reader.pid, Start: 1, Args: []string{"codex", "--no-daemon"}}
	s := NewShim(realSys{f, dir})
	if err := s.AcceptDurable(Claim{SessionID: "T1"}); !errors.Is(err, ErrSecondOpener) {
		t.Fatalf("%v", err)
	}
}

func TestReal_OwnerProofLockFreeButParentHasFileOpen_ExpectLockFree(t *testing.T) {
	needLsof(t)
	dir, path := lockFile(t)
	reader := startHelper(t, "read", path)
	f := newFake()
	f.parent = Proc{PID: reader.pid, Start: 1, Args: []string{"codex", "--no-daemon"}}
	s := NewShim(realSys{f, dir})
	if err := s.AcceptDurable(Claim{SessionID: "T1"}); !errors.Is(err, ErrLockFree) {
		t.Fatalf("%v", err)
	}
}
