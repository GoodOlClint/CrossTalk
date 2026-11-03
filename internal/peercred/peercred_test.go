package peercred

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"golang.org/x/sys/unix"
)

const childEnv = "PEERCRED_TEST_DIAL"

func TestMain(m *testing.M) {
	if path := os.Getenv(childEnv); path != "" {
		c, err := net.Dial("unix", path)
		if err != nil {
			os.Exit(3)
		}
		_, _ = c.Write([]byte(strconv.Itoa(os.Getpid()) + "\n"))
		buf := make([]byte, 1)
		_, _ = c.Read(buf)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func shortSockPath(t *testing.T) string {
	dir, err := os.MkdirTemp("", "pc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "s.sock")
}

func TestPeerPIDListenerSameProcess(t *testing.T) {
	path := shortSockPath(t)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	errc := make(chan error, 1)
	go func() {
		c, err := net.Dial("unix", path)
		if err == nil {
			defer c.Close()
			_, err = c.Read(make([]byte, 1))
		}
		errc <- err
	}()
	conn, err := ln.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	pid, err := PeerPID(conn)
	conn.Close()
	<-errc
	if err != nil {
		t.Fatalf("PeerPID: %v", err)
	}
	t.Logf("accepted peer pid=%d os.Getpid()=%d", pid, os.Getpid())
	if pid != os.Getpid() {
		t.Fatalf("PeerPID = %d, want %d", pid, os.Getpid())
	}
}

func TestPeerPIDSocketpair(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	f := os.NewFile(uintptr(fds[0]), "sp0")
	defer f.Close()
	defer unix.Close(fds[1])
	c, err := net.FileConn(f)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	pid, err := PeerPID(c.(*net.UnixConn))
	if err != nil {
		t.Fatalf("PeerPID: %v", err)
	}
	t.Logf("socketpair peer pid=%d os.Getpid()=%d", pid, os.Getpid())
	if pid != os.Getpid() {
		t.Fatalf("PeerPID = %d, want %d", pid, os.Getpid())
	}
}

func TestPeerPIDOtherProcess(t *testing.T) {
	path := shortSockPath(t)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), childEnv+"="+path)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	conn, err := ln.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	pid, perr := PeerPID(conn)
	buf := make([]byte, 32)
	n, _ := conn.Read(buf)
	conn.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("child: %v", err)
	}
	if perr != nil {
		t.Fatalf("PeerPID: %v", perr)
	}
	t.Logf("child peer pid=%d child reported=%q cmd.Process.Pid=%d", pid, string(buf[:n]), cmd.Process.Pid)
	if pid != cmd.Process.Pid || pid == os.Getpid() {
		t.Fatalf("PeerPID = %d, want child %d", pid, cmd.Process.Pid)
	}
	if want := strconv.Itoa(cmd.Process.Pid) + "\n"; string(buf[:n]) != want {
		t.Fatalf("child reported %q, want %q", buf[:n], want)
	}
}
