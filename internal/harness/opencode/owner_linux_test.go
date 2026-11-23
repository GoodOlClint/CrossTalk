package opencode

import (
	"context"
	"net"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestOwnerUnacceptedDeferAcceptConnection(t *testing.T) {
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) {
			serr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_DEFER_ACCEPT, 30)
		}); err != nil {
			return err
		}
		return serr
	}}
	ln, err := lc.Listen(context.Background(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	oldPIDs, oldWait := peerPIDs, peerWait
	peerPIDs, peerWait = osPeerPIDs, 400*time.Millisecond
	t.Cleanup(func() { peerPIDs, peerWait = oldPIDs, oldWait })

	conn, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	start := time.Now()
	if msg := peerRefusal(context.Background(), conn, os.Getpid()); msg != "" {
		t.Fatalf("connection to the service's own listener, not yet accepted: refused after %v: %s", time.Since(start), msg)
	}
	if msg := peerRefusal(context.Background(), conn, os.Getpid()+1); msg == "" {
		t.Fatal("connection whose listener is held by another pid was accepted as the service's")
	}
}
