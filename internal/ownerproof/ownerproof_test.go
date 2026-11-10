package ownerproof

import (
	"bufio"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/peercred"
)

const (
	helperMode = "OWNERPROOF_HELPER_MODE"
	helperPath = "OWNERPROOF_HELPER_PATH"
)

// The test binary re-executed is the child process: "hold" opens and flocks
// the file, "read" only opens it, "dial" connects to the socket at the path.
func TestMain(m *testing.M) {
	if mode := os.Getenv(helperMode); mode != "" {
		path := os.Getenv(helperPath)
		if mode == "dial" {
			c, err := net.Dial("unix", path)
			if err != nil {
				os.Exit(2)
			}
			c.Read(make([]byte, 1))
			os.Exit(0)
		}
		f, err := os.OpenFile(path, os.O_RDWR, 0)
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

func supported(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("ownerproof is darwin and linux only")
	}
}

type helper struct {
	cmd *exec.Cmd
	pid int
}

func startHelper(t *testing.T, mode, path string) *helper {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), helperMode+"="+mode, helperPath+"="+path)
	stdin, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "ready") {
		t.Fatalf("helper %s did not start: %q %v", mode, line, err)
	}
	t.Cleanup(func() { stdin.Close(); cmd.Process.Kill(); cmd.Wait() })
	return &helper{cmd: cmd, pid: cmd.Process.Pid}
}

func lockFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "T1.lock")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRealLockHeld_FreeHeldThenHolderKilled_ExpectFreeHeldFree(t *testing.T) {
	supported(t)
	path := lockFile(t)
	if held, err := LockHeld(path); err != nil || held {
		t.Fatalf("free file: held=%v err=%v", held, err)
	}
	h := startHelper(t, "hold", path)
	if held, err := LockHeld(path); err != nil || !held {
		t.Fatalf("held file: held=%v err=%v", held, err)
	}
	h.cmd.Process.Signal(syscall.SIGKILL)
	h.cmd.Wait()
	deadline := time.Now().Add(2 * time.Second)
	for {
		held, err := LockHeld(path)
		if err == nil && !held {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("after kill -9: held=%v err=%v", held, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRealLockHeld_MissingFile_ExpectError(t *testing.T) {
	supported(t)
	held, err := LockHeld(filepath.Join(t.TempDir(), "missing.lock"))
	if err == nil || held {
		t.Fatalf("missing file: held=%v err=%v, want an error", held, err)
	}
}

func TestRealOpeners_HolderAndReaderChildren_ExpectBothSelfExcluded(t *testing.T) {
	supported(t)
	path := lockFile(t)
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	holder := startHelper(t, "hold", path)
	reader := startHelper(t, "read", path)
	other := lockFile(t)
	startHelper(t, "read", other)
	start := time.Now()
	got, err := Openers(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Openers took %v", time.Since(start))
	slices.Sort(got)
	want := []int{holder.pid, reader.pid}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("openers %v, want %v (self %d excluded)", got, want, os.Getpid())
	}
}

func TestRealOpeners_SameNameDifferentInode_ExpectNotCounted(t *testing.T) {
	supported(t)
	path := lockFile(t)
	startHelper(t, "read", path)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Openers(path)
	if err != nil || len(got) != 0 {
		t.Fatalf("openers of the new inode %v err=%v, want none", got, err)
	}
}

func TestRealOpeners_MissingFile_ExpectError(t *testing.T) {
	supported(t)
	if _, err := Openers(filepath.Join(t.TempDir(), "missing.lock")); err == nil {
		t.Fatal("missing file must be an error")
	}
}

func TestRealInfo_Child_ExpectParentIsSelfAndStartStable(t *testing.T) {
	supported(t)
	h := startHelper(t, "read", lockFile(t))
	p, err := Info(h.pid)
	if err != nil {
		t.Fatal(err)
	}
	if p.PID != h.pid || p.PPID != os.Getpid() || p.Start.IsZero() {
		t.Fatalf("info %+v, want ppid %d", p, os.Getpid())
	}
	again, err := Info(h.pid)
	if err != nil || !again.Start.Equal(p.Start) {
		t.Fatalf("start not stable: %v vs %v (%v)", again.Start, p.Start, err)
	}
	exe, argv, err := Args(h.pid)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(exe) != filepath.Base(os.Args[0]) || len(argv) == 0 {
		t.Fatalf("exe %q argv %q", exe, argv)
	}
}

func TestRealInfo_ExitedPID_ExpectErrNoProcess(t *testing.T) {
	supported(t)
	h := startHelper(t, "read", lockFile(t))
	h.cmd.Process.Kill()
	h.cmd.Wait()
	if _, err := Info(h.pid); !errors.Is(err, ErrNoProcess) {
		t.Fatalf("err %v, want ErrNoProcess", err)
	}
}

func TestRealAncestry_PeerPIDOverUnixSocket_ExpectChainReachesSelf(t *testing.T) {
	supported(t)
	dir, err := os.MkdirTemp("", "op")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), helperMode+"=dial", helperPath+"="+sock)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	conn, err := ln.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	pid, err := peercred.PeerPID(conn)
	if err != nil || pid != cmd.Process.Pid {
		t.Fatalf("peer pid %d err %v, want %d", pid, err, cmd.Process.Pid)
	}
	chain, err := Ancestry(pid)
	if err != nil {
		t.Fatal(err)
	}
	self, err := Info(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if chain[0].PID != pid || !slices.ContainsFunc(chain, self.Same) {
		t.Fatalf("chain %+v does not reach self %+v", chain, self)
	}
}
