package remote

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

const wantHint = "a host firewall or network filter (for example an application firewall such as Little Snitch) on either host may be holding the connection"

func holdServer(t *testing.T, closeAtOnce bool) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var held []net.Conn
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			if closeAtOnce {
				c.Close()
			} else {
				held = append(held, c)
			}
		}
	}()
	t.Cleanup(func() {
		l.Close()
		<-done
		for _, c := range held {
			c.Close()
		}
	})
	return l.Addr().String()
}

func TestStalledHandshakeLogsAddressOnce(t *testing.T) {
	for _, mode := range []string{"stall", "close"} {
		t.Run(mode, func(t *testing.T) {
			e := startGateway(t, func(c *GatewayConfig) { c.Limits.HandshakeTimeout = 300 * time.Millisecond })
			var conns []net.Conn
			for i := 0; i < 2; i++ {
				c, err := net.DialTimeout("tcp", e.addr(), 2*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				conns = append(conns, c)
				if mode == "close" {
					c.Close()
				}
			}
			defer func() {
				for _, c := range conns {
					c.Close()
				}
			}()
			time.Sleep(900 * time.Millisecond)
			if got, want := withoutStart(e.logs.String()), "handshake not completed from 127.0.0.1\n"; got != want {
				t.Fatalf("log = %q, want exactly %q (one line, address only, one per address in the window)", got, want)
			}
		})
	}
}

func TestStalledHandshakeLogsAgainAfterWindow(t *testing.T) {
	e := startGateway(t, func(c *GatewayConfig) {
		c.Limits.HandshakeTimeout = 100 * time.Millisecond
		c.Limits.StallWindow = 300 * time.Millisecond
	})
	for i := 0; i < 2; i++ {
		c, err := net.DialTimeout("tcp", e.addr(), 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(600 * time.Millisecond)
		c.Close()
	}
	if got, want := withoutStart(e.logs.String()), strings.Repeat("handshake not completed from 127.0.0.1\n", 2); got != want {
		t.Fatalf("log = %q, want %q", got, want)
	}
}

func TestStallLimiterBounded(t *testing.T) {
	const max = 64
	l := newStallLimiter(time.Minute, max)
	now := time.Unix(1000, 0)
	allowed := 0
	for i := 0; i < 10000; i++ {
		if l.allow(fmt.Sprintf("10.%d.%d.%d", i>>16&255, i>>8&255, i&255), now) {
			allowed++
		}
	}
	if allowed > max || l.size() > max {
		t.Fatalf("allowed %d, table %d, want at most %d each", allowed, l.size(), max)
	}
	if allowed == 0 {
		t.Fatal("nothing was ever allowed")
	}
	if l.allow("10.0.0.0", now.Add(time.Second)) {
		t.Fatal("same address allowed twice inside the window")
	}
	if !l.allow("192.0.2.1", now.Add(2*time.Minute)) {
		t.Fatal("a new address was refused after every entry expired")
	}
	if l.size() > max {
		t.Fatalf("table %d after expiry, want at most %d", l.size(), max)
	}
}

func TestClientDoReportsStalledHandshake(t *testing.T) {
	for _, closeAtOnce := range []bool{false, true} {
		t.Run(fmt.Sprintf("close=%v", closeAtOnce), func(t *testing.T) {
			addr := holdServer(t, closeAtOnce)
			host, err := LoadOrCreateHost(newDir(t))
			if err != nil {
				t.Fatal(err)
			}
			c := NewClient(host, Peer{Alias: "p", Address: addr, Fingerprint: strings.Repeat("ab", 32)})
			c.Timeout = 400 * time.Millisecond
			start := time.Now()
			_, _, err = c.Do(context.Background(), "GET", agentsPath, nil, nil)
			if time.Since(start) > 3*time.Second {
				t.Fatalf("took %v", time.Since(start))
			}
			assertStalled(t, err)
			if e, ok := err.(*Error); !ok || e.Reason != ReasonTimeout {
				t.Fatalf("error %#v, want reason %s", err, ReasonTimeout)
			}
		})
	}
}

func TestJoinReportsStalledHandshake(t *testing.T) {
	for _, closeAtOnce := range []bool{false, true} {
		t.Run(fmt.Sprintf("close=%v", closeAtOnce), func(t *testing.T) {
			addr := holdServer(t, closeAtOnce)
			host, port, _ := net.SplitHostPort(addr)
			var p int
			fmt.Sscan(port, &p)
			h, err := LoadOrCreateHost(newDir(t))
			if err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			_, err = JoinGateway(context.Background(), JoinOptions{
				Host: h, Config: Config{Version: 1, Alias: "beta", Bind: "127.0.0.1", Port: 7746}, Peers: NewPeers(newDir(t)),
				Token:   PairToken{Address: host, Port: p, Fingerprint: strings.Repeat("cd", 32), ID: "id", Secret: "c2VjcmV0"},
				Timeout: 3 * time.Second, HandshakeTimeout: 300 * time.Millisecond,
			})
			if time.Since(start) > 1500*time.Millisecond {
				t.Fatalf("join took %v, want within the handshake deadline", time.Since(start))
			}
			assertStalled(t, err)
		})
	}
}

func assertStalled(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("no error")
	}
	s := err.Error()
	if !strings.Contains(s, "handshake") || !strings.Contains(s, wantHint) {
		t.Fatalf("error %q lacks the word handshake or the firewall hint", s)
	}
}

func withoutStart(log string) string {
	var b strings.Builder
	for _, l := range strings.SplitAfter(log, "\n") {
		if !strings.HasPrefix(l, "gateway listening on ") {
			b.WriteString(l)
		}
	}
	return b.String()
}
