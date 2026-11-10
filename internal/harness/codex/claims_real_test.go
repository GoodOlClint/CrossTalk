package codex

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
)

const (
	realHelperMode = "CODEX_CLAIMS_HELPER_MODE"
	realHelperPath = "CODEX_CLAIMS_HELPER_PATH"
)

// The test binary re-executed stands in for a codex process: "hold" opens
// and flocks the lock file as Codex does, "read" only opens it.
func TestMain(m *testing.M) {
	if mode := os.Getenv(realHelperMode); mode != "" {
		f, err := os.OpenFile(os.Getenv(realHelperPath), os.O_RDWR, 0)
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

func realKernel(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("owner proof reads are darwin and linux only")
	}
}

func startRealHelper(t *testing.T, mode, path string) int {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), realHelperMode+"="+mode, realHelperPath+"="+path)
	stdin, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "ready") {
		t.Fatalf("helper %s: %q %v", mode, line, err)
	}
	t.Cleanup(func() { stdin.Close(); cmd.Process.Kill(); cmd.Wait() })
	return cmd.Process.Pid
}

func realClaims(t *testing.T, parentPID int, dir string) (*Claims, *fakeRegistrar) {
	t.Helper()
	parent, err := Kernel{}.Info(parentPID)
	if err != nil {
		t.Fatal(err)
	}
	reg := &fakeRegistrar{}
	return NewClaims(ClaimOptions{Parent: parent, LockDir: dir, Registrar: reg, Tries: 2, Interval: 1}), reg
}

func realLock(t *testing.T) (dir, path string) {
	t.Helper()
	dir = t.TempDir()
	path = lockPath(dir, "T1")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, path
}

func TestRealProof_ChildHoldsLockAsParent_ExpectBound(t *testing.T) {
	realKernel(t)
	dir, path := realLock(t)
	codex := startRealHelper(t, "hold", path)
	c, reg := realClaims(t, codex, dir)
	if err := c.AcceptDurable(context.Background(), claim("T1")); err != nil {
		t.Fatal(err)
	}
	if !c.Bound("T1") || len(reg.got()) != 1 {
		t.Fatal("not bound")
	}
}

func TestRealProof_ReaderChildAlsoOpen_ExpectSecondOpener(t *testing.T) {
	realKernel(t)
	dir, path := realLock(t)
	codex := startRealHelper(t, "hold", path)
	startRealHelper(t, "read", path)
	c, _ := realClaims(t, codex, dir)
	if err := c.AcceptDurable(context.Background(), claim("T1")); !errors.Is(err, ErrSecondOpener) {
		t.Fatalf("err %v, want ErrSecondOpener", err)
	}
}

func TestRealProof_ParentOnlyReadsLockHeldByOther_ExpectRejected(t *testing.T) {
	realKernel(t)
	dir, path := realLock(t)
	startRealHelper(t, "hold", path)
	codex := startRealHelper(t, "read", path)
	c, _ := realClaims(t, codex, dir)
	if err := c.AcceptDurable(context.Background(), claim("T1")); !errors.Is(err, ErrSecondOpener) {
		t.Fatalf("err %v, want ErrSecondOpener", err)
	}
}

func TestRealProof_LockFreeParentHasFileOpen_ExpectLockFree(t *testing.T) {
	realKernel(t)
	dir, path := realLock(t)
	codex := startRealHelper(t, "read", path)
	c, _ := realClaims(t, codex, dir)
	if err := c.AcceptDurable(context.Background(), claim("T1")); !errors.Is(err, ErrLockFree) {
		t.Fatalf("err %v, want ErrLockFree", err)
	}
}

func TestRealProof_HolderKilled_ExpectRevokedOnRecheck(t *testing.T) {
	realKernel(t)
	dir, path := realLock(t)
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), realHelperMode+"=hold", realHelperPath+"="+path)
	stdin, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	if line, _ := bufio.NewReader(out).ReadString('\n'); line != "ready\n" {
		t.Fatalf("helper: %q", line)
	}
	c, _ := realClaims(t, cmd.Process.Pid, dir)
	if err := c.AcceptDurable(context.Background(), claim("T1")); err != nil {
		t.Fatal(err)
	}
	cmd.Process.Signal(syscall.SIGKILL)
	cmd.Wait()
	if got := c.Recheck(); len(got) != 1 || c.Bound("T1") {
		t.Fatalf("revoked %v after the owner was killed", got)
	}
}

func TestRealProof_ConcurrentProofsOfFreeLockFromTwoTables_ExpectNoFalsePass(t *testing.T) {
	realKernel(t)
	dir, path := realLock(t)
	codex := startRealHelper(t, "read", path)
	a, _ := realClaims(t, codex, dir)
	b, _ := realClaims(t, codex, dir)
	a.opt.Tries, b.opt.Tries = 1, 1
	var pass atomic.Int64
	var wg sync.WaitGroup
	for g := range 8 {
		c := a
		if g%2 == 1 {
			c = b
		}
		wg.Go(func() {
			for range 200 {
				if c.proveTarget("T1") == nil {
					pass.Add(1)
				}
			}
		})
	}
	wg.Wait()
	if n := pass.Load(); n != 0 {
		t.Fatalf("%d of 1600 proofs passed on a free lock", n)
	}
}
