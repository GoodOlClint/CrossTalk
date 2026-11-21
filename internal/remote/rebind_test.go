package remote

import (
	"errors"
	"net"
	"net/netip"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type countingListener struct {
	net.Listener
	active, max *atomic.Int32
}

func (l countingListener) Accept() (net.Conn, error) {
	n := l.active.Add(1)
	defer l.active.Add(-1)
	for {
		m := l.max.Load()
		if n <= m || l.max.CompareAndSwap(m, n) {
			break
		}
	}
	return l.Listener.Accept()
}

func TestAcceptLoopReturnsWhenListenerCloses(t *testing.T) {
	e := newGateway(t, nil)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { e.gw.acceptLoop(ln); close(done) }()
	ln.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("acceptLoop still running after its listener closed")
	}
}

func TestRebindInPlace(t *testing.T) {
	var gone atomic.Bool
	var active, max atomic.Int32
	e := newGateway(t, func(c *GatewayConfig) {
		c.Limits.AddrPoll = 5 * time.Millisecond
		c.Limits.BindRetry = 5 * time.Millisecond
		c.Addrs = func() ([]net.Addr, error) {
			if gone.Load() {
				return []net.Addr{&net.IPNet{IP: net.ParseIP("192.0.2.9"), Mask: net.CIDRMask(24, 32)}}, nil
			}
			return []net.Addr{&net.IPNet{IP: net.ParseIP("127.0.0.1"), Mask: net.CIDRMask(8, 32)}}, nil
		}
		c.Listen = func(network, addr string) (net.Listener, error) {
			if gone.Load() {
				return nil, &net.OpError{Op: "listen", Net: network, Err: os.NewSyscallError("bind", syscall.EADDRNOTAVAIL)}
			}
			ln, err := net.Listen(network, addr)
			if err != nil {
				return nil, err
			}
			return countingListener{ln, &active, &max}, nil
		}
	}).start()

	client := e.newClient(true)
	open, err := e.dial(clientTLS(client, 0))
	if err != nil {
		t.Fatal(err)
	}
	defer open.Close()
	if code, err := roundTrip(open); err != nil || code != 204 {
		t.Fatalf("before the address left: %d, %v", code, err)
	}

	gone.Store(true)
	waitFor(t, "waiting-for-address state", func() bool { return e.gw.State() == StateWaitingAddr })
	open.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := open.Read(make([]byte, 1)); err == nil {
		t.Fatal("an established connection survived the listener closing")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatalf("an established connection was left open: %v", err)
	}

	gone.Store(false)
	waitFor(t, "listening state", func() bool { return e.gw.State() == stateListening })
	c, err := e.dial(clientTLS(client, 0))
	if err != nil {
		t.Fatalf("dial after the address returned: %v", err)
	}
	defer c.Close()
	if code, err := roundTrip(c); err != nil || code != 204 {
		t.Fatalf("after the address returned: %d, %v", code, err)
	}
	if max.Load() != 1 {
		t.Fatalf("up to %d accept loops ran at once, want 1", max.Load())
	}
}

func TestHostHasAddr(t *testing.T) {
	list := func() ([]net.Addr, error) {
		return []net.Addr{&net.IPNet{IP: net.ParseIP("100.64.0.5"), Mask: net.CIDRMask(10, 32)}}, nil
	}
	if ok, _ := hostHasAddr(netip.MustParseAddr("100.64.0.5"), list); !ok {
		t.Error("an assigned address reads as absent")
	}
	if ok, _ := hostHasAddr(netip.MustParseAddr("100.64.0.6"), list); ok {
		t.Error("an unassigned address reads as present")
	}
	if ok, err := hostHasAddr(netip.MustParseAddr("100.64.0.5"), func() ([]net.Addr, error) { return nil, errors.New("x") }); ok || err == nil {
		t.Error("a listing error is not reported")
	}
}
