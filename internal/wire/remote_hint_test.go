package wire

import (
	"context"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/a2a"
	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/remote"
)

const wantStallHint = "a host firewall or network filter (for example an application firewall such as Little Snitch) on either host may be holding the connection"

func stallText(addr string) string {
	return "handshake timeout: no TLS handshake completed with " + addr + "; " + wantStallHint
}

// silentListener accepts TCP connections and never starts a TLS handshake.
// With passFirst, the first connection is piped to target and later ones are held.
func silentListener(t *testing.T, target string, passFirst bool) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var n atomic.Int32
	stop := make(chan struct{})
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			if passFirst && n.Add(1) == 1 {
				up, err := net.Dial("tcp", target)
				if err != nil {
					c.Close()
					continue
				}
				go func() { io.Copy(up, c); up.Close() }()
				go func() { io.Copy(c, up); c.Close() }()
				continue
			}
			go func() { <-stop; c.Close() }()
		}
	}()
	t.Cleanup(func() { close(stop); l.Close() })
	return l.Addr().String()
}

func repoint(t *testing.T, h *rhost, alias, addr, fp string) {
	t.Helper()
	if err := h.peers.Upsert(remote.Peer{Alias: alias, Address: addr, Fingerprint: fp}); err != nil {
		t.Fatal(err)
	}
}

func TestRemoteSendListingStallCarriesHint(t *testing.T) {
	for _, tool := range []string{"send_message", "ask"} {
		t.Run(tool, func(t *testing.T) {
			h1, h2, alice, _ := twoHosts(t)
			addr := silentListener(t, "", false)
			repoint(t, h1, "lab", addr, h2.host.Fingerprint)
			out := alice.ok(tool, map[string]any{"to": "bob@lab", "text": "hi"})
			if str(out, "state") != "unreachable" || str(out, "reason") != "timeout" {
				t.Fatalf("%v", out)
			}
			if got := str(out, "hint"); got != stallText(addr) {
				t.Fatalf("hint %q, want %q", got, stallText(addr))
			}
		})
	}
}

func TestRemoteSendStallAfterListingCarriesHint(t *testing.T) {
	h1, h2, alice, _ := twoHosts(t)
	addr := silentListener(t, h2.addr(), true)
	repoint(t, h1, "lab", addr, h2.host.Fingerprint)
	out := alice.ok("send_message", map[string]any{"to": "bob@lab", "text": "hi"})
	if str(out, "state") != "unreachable" || str(out, "reason") != "timeout" {
		t.Fatalf("%v", out)
	}
	if got := str(out, "hint"); got != stallText(addr) {
		t.Fatalf("hint %q, want %q", got, stallText(addr))
	}
}

func TestRemoteReplyStallCarriesHint(t *testing.T) {
	h1, h2, alice, bob := twoHosts(t)
	askID := askBob(t, alice, nil)
	addr := silentListener(t, "", false)
	repoint(t, h2, "desk", addr, h1.host.Fingerprint)
	out := bob.ok("reply", replyArgs(askID))
	if str(out, "state") != "unreachable" || str(out, "reason") != "remote_reply_pending" {
		t.Fatalf("reply %v, want unreachable/remote_reply_pending", out)
	}
	if got := str(out, "hint"); got != stallText(addr) {
		t.Fatalf("reply %v, hint %q, want %q", out, got, stallText(addr))
	}
}

func TestPollSettledStallKeepsPendingAndCause(t *testing.T) {
	h1, h2, alice, _ := twoHosts(t)
	_ = alice
	addr := silentListener(t, "", false)
	peer := remote.Peer{Alias: "lab", Address: addr, Fingerprint: h2.host.Fingerprint}
	c := &a2a.Client{HTTP: remote.NewClient(h1.host, peer).HTTPClient()}
	_, state, err := pollSettled(context.Background(), c, "https://"+addr+"/v1/a2a/x", "", "m", 1*time.Second)
	var he *remote.HandshakeError
	if state != adapter.Unknown || err == nil || err.Error() != ReasonPending || !errors.As(err, &he) {
		t.Fatalf("state %v err %v, want unknown, pending, carrying a HandshakeError", state, err)
	}
	if he.Error() != stallText(addr) {
		t.Fatalf("cause %q, want %q", he.Error(), stallText(addr))
	}
}
