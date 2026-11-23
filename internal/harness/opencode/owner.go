package opencode

import (
	"context"
	"net"
	"net/netip"
	"time"
)

// peerPIDs returns the pid of every process this user can see holding the TCP
// socket whose local end is svc and remote end is self. A connection the
// service has not accepted yet has no holder.
var peerPIDs = osPeerPIDs

// peerWait bounds the retries while the service has not accepted the
// connection yet.
var peerWait = time.Second

// nudgeAfter is how long an unaccepted connection waits before peerRefusal
// sends one empty line. A service that sets TCP_DEFER_ACCEPT (OpenCode on
// Linux) accepts an idle connection only after about a second; HTTP ignores a
// leading empty line and it carries no credential.
const nudgeAfter = 100 * time.Millisecond

type servicePID struct{}

type ownerError struct{ msg string }

func (e *ownerError) Error() string { return e.msg }

func dialService(ctx context.Context, network, addr string) (net.Conn, error) {
	pid, _ := ctx.Value(servicePID{}).(int)
	if pid <= 0 {
		return nil, &ownerError{"no service pid for this request; nothing sent"}
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	if msg := peerRefusal(ctx, conn, pid); msg != "" {
		conn.Close()
		return nil, &ownerError{msg}
	}
	return conn, nil
}

func peerRefusal(ctx context.Context, conn net.Conn, pid int) string {
	self, err1 := netip.ParseAddrPort(conn.LocalAddr().String())
	svc, err2 := netip.ParseAddrPort(conn.RemoteAddr().String())
	if err1 != nil || err2 != nil {
		return "service connection addresses unreadable; nothing sent"
	}
	self = netip.AddrPortFrom(self.Addr().Unmap(), self.Port())
	svc = netip.AddrPortFrom(svc.Addr().Unmap(), svc.Port())
	start := time.Now()
	deadline := start.Add(peerWait)
	nudged := false
	for {
		pids, err := peerPIDs(ctx, self, svc)
		if err == nil && len(pids) > 0 {
			for _, p := range pids {
				if p != pid {
					return "service connection is held by a process other than the service pid; nothing sent"
				}
			}
			return ""
		}
		if !time.Now().Before(deadline) {
			return "holder of the service connection could not be determined; nothing sent"
		}
		if !nudged && time.Since(start) >= nudgeAfter {
			nudged = true
			if _, err := conn.Write([]byte("\r\n")); err != nil {
				return "holder of the service connection could not be determined; nothing sent"
			}
		}
		select {
		case <-ctx.Done():
			return "holder of the service connection could not be determined; nothing sent"
		case <-time.After(20 * time.Millisecond):
		}
	}
}
