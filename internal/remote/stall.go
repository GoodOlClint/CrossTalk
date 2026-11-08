package remote

import (
	"errors"
	"io"
	"net"
	"sync"
	"syscall"
	"time"
)

// stallLimiter admits at most one log line per address per window and at most
// max addresses per window, so a scanner cannot grow the table or flood the log.
type stallLimiter struct {
	mu     sync.Mutex
	seen   map[string]time.Time
	window time.Duration
	max    int
}

func newStallLimiter(window time.Duration, max int) *stallLimiter {
	return &stallLimiter{seen: map[string]time.Time{}, window: window, max: max}
}

func (s *stallLimiter) allow(addr string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.seen[addr]; ok && now.Sub(t) < s.window {
		return false
	}
	if len(s.seen) >= s.max {
		for a, t := range s.seen {
			if now.Sub(t) >= s.window {
				delete(s.seen, a)
			}
		}
		if len(s.seen) >= s.max {
			return false
		}
	}
	s.seen[addr] = now
	return true
}

func (s *stallLimiter) size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.seen)
}

// isStall reports a handshake that ended by timeout or by the peer closing
// before sending anything.
func isStall(err error) bool {
	var ne net.Error
	return errors.Is(err, io.EOF) || errors.As(err, &ne) && ne.Timeout()
}

// handshakeCut is isStall plus a reset, which a closing filter can send in
// place of a close.
func handshakeCut(err error) bool {
	return isStall(err) || errors.Is(err, syscall.ECONNRESET)
}

func (g *Gateway) noteStall(raw net.Conn, err error) {
	if !isStall(err) {
		return
	}
	host, _, e := net.SplitHostPort(raw.RemoteAddr().String())
	if e != nil {
		return
	}
	if g.stalls.allow(host, g.cfg.Now()) {
		g.cfg.Log("handshake not completed from %s", host)
	}
}
