package appserver

import (
	"bufio"
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func pipeClient(t *testing.T, writeTimeout time.Duration) (*Client, net.Conn) {
	t.Helper()
	old := WriteTimeout
	WriteTimeout = writeTimeout
	t.Cleanup(func() { WriteTimeout = old })
	local, remote := net.Pipe()
	c := newClient(local, bufio.NewReader(local), Options{}, "")
	go c.readLoop()
	t.Cleanup(func() {
		remote.Close()
		c.Close()
	})
	return c, remote
}

func readAll(t *testing.T, conn net.Conn, within time.Duration) int {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(within))
	total := 0
	buf := make([]byte, 64<<10)
	for {
		n, err := conn.Read(buf)
		total += n
		if err != nil {
			return total
		}
	}
}

func TestCallRawWriteBoundedByCallerDeadline(t *testing.T) {
	c, remote := pipeClient(t, 2*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.CallRaw(ctx, "turn/start", strings.Repeat("x", 1024))
	if el := time.Since(start); el > 300*time.Millisecond {
		t.Fatalf("CallRaw took %v with a 30ms caller deadline", el)
	}
	if !errors.Is(err, ErrNotSent) {
		t.Fatalf("err = %v, want ErrNotSent", err)
	}
	if n := readAll(t, remote, 100*time.Millisecond); n != 0 {
		t.Fatalf("peer received %d bytes after the caller deadline", n)
	}
}

func TestCallRawLockWaitBoundedByCallerDeadline(t *testing.T) {
	c, remote := pipeClient(t, time.Second)
	notified := make(chan struct{})
	go func() {
		defer close(notified)
		c.Notify("x", strings.Repeat("a", 1<<20))
	}()
	defer func() {
		remote.Close()
		<-notified
	}()
	time.Sleep(20 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.CallRaw(ctx, "turn/start", nil)
	if el := time.Since(start); el > 300*time.Millisecond {
		t.Fatalf("CallRaw waited %v for the write lock with a 30ms caller deadline", el)
	}
	if !errors.Is(err, ErrNotSent) {
		t.Fatalf("err = %v, want ErrNotSent", err)
	}
}

func TestCallRawCanceledSendsNothing(t *testing.T) {
	c, remote := pipeClient(t, 2*time.Second)
	got := make(chan int, 1)
	go func() { got <- readAll(t, remote, 200*time.Millisecond) }()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.CallRaw(ctx, "turn/start", map[string]string{"text": "late"})
	if !errors.Is(err, ErrNotSent) || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want ErrNotSent wrapping context.Canceled", err)
	}
	if n := <-got; n != 0 {
		t.Fatalf("peer received %d bytes from a call canceled before send", n)
	}
}

func TestCloseContextBoundedByCallerDeadline(t *testing.T) {
	c, remote := pipeClient(t, 2*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	c.CloseContext(ctx)
	if el := time.Since(start); el > 300*time.Millisecond {
		t.Fatalf("CloseContext took %v with a 30ms caller deadline", el)
	}
	if n := readAll(t, remote, 100*time.Millisecond); n != 0 {
		t.Fatalf("peer received %d bytes after the caller deadline", n)
	}
	expired, cancel2 := context.WithCancel(context.Background())
	cancel2()
	c2, remote2 := pipeClient(t, 2*time.Second)
	got := make(chan int, 1)
	go func() { got <- readAll(t, remote2, 200*time.Millisecond) }()
	c2.CloseContext(expired)
	if n := <-got; n != 0 {
		t.Fatalf("peer received %d bytes from a close after cancellation", n)
	}
}

type cancelDuringDeadlineConn struct {
	net.Conn
	cancel  context.CancelFunc
	expired chan struct{}
	once    sync.Once
	armed   atomic.Bool
}

func (c *cancelDuringDeadlineConn) SetWriteDeadline(d time.Time) error {
	if d.After(time.Now()) && !c.armed.Swap(true) {
		c.cancel()
		select {
		case <-c.expired:
		case <-time.After(20 * time.Millisecond):
		}
		return c.Conn.SetWriteDeadline(d)
	}
	err := c.Conn.SetWriteDeadline(d)
	if !d.After(time.Now()) {
		c.once.Do(func() { close(c.expired) })
	}
	return err
}

func TestCallRawCanceledWhileDeadlineSetSendsNothing(t *testing.T) {
	old := WriteTimeout
	WriteTimeout = 2 * time.Second
	t.Cleanup(func() { WriteTimeout = old })
	local, remote := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn := &cancelDuringDeadlineConn{Conn: local, cancel: cancel, expired: make(chan struct{})}
	c := newClient(conn, bufio.NewReader(conn), Options{}, "")
	go c.readLoop()
	t.Cleanup(func() {
		remote.Close()
		c.Close()
	})
	start := time.Now()
	_, err := c.CallRaw(ctx, "turn/start", map[string]string{"text": "late"})
	if el := time.Since(start); el > 200*time.Millisecond {
		t.Fatalf("CallRaw took %v after cancellation during deadline setup; WriteTimeout is %v", el, WriteTimeout)
	}
	if !errors.Is(err, ErrNotSent) {
		t.Fatalf("err = %v, want ErrNotSent", err)
	}
	if n := readAll(t, remote, 100*time.Millisecond); n != 0 {
		t.Fatalf("peer received %d bytes from a call canceled before send", n)
	}
}
