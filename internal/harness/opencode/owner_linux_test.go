package opencode

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestOwnerDeferAcceptServiceIsChecked(t *testing.T) {
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
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()

	oldPIDs, oldWait := peerPIDs, peerWait
	peerPIDs, peerWait = osPeerPIDs, 500*time.Millisecond
	t.Cleanup(func() { peerPIDs, peerWait = oldPIDs, oldWait })

	dial := func(pid int) (net.Conn, error) {
		ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), servicePID{}, pid), 5*time.Second)
		defer cancel()
		return dialService(ctx, "tcp4", ln.Addr().String())
	}
	conn, err := dial(os.Getpid())
	if err != nil {
		t.Fatalf("service that accepts only once data arrives: %v", err)
	}
	conn.Close()

	conn, err = dial(os.Getpid() + 1)
	var oe *ownerError
	if !errors.As(err, &oe) {
		if conn != nil {
			conn.Close()
		}
		t.Fatalf("holder other than the service pid: err %v, want an ownerError", err)
	}
	if !strings.Contains(oe.msg, "held by a process other than") {
		t.Errorf("ownerError message %q does not name a foreign holder", oe.msg)
	}
}
