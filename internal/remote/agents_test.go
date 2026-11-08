package remote

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

var sampleRows = []AgentRow{
	{AgentID: "aaaaaaaaaaaaaaaa", Name: "bob", Harness: "claude", CrossTalk: true, Role: "primary", Reachable: true, Status: "idle", Messageable: true},
	{AgentID: "bbbbbbbbbbbbbbbb", Name: "plain", Harness: "codex", Status: "loaded", Note: NativeNote},
}

func agentsGateway(t *testing.T, calls *atomic.Int32) *gwEnv {
	t.Helper()
	src := func(context.Context) []AgentRow {
		if calls != nil {
			calls.Add(1)
		}
		return sampleRows
	}
	return startGateway(t, func(c *GatewayConfig) {
		c.Handler = NewMux(NewForwarder(c.StateDir, agentsOf(nil)), src)
	})
}

func peerOf(e *gwEnv) Peer {
	return Peer{Alias: "gw", Address: e.addr(), Fingerprint: e.host.Fingerprint}
}

func TestAgentsEndpointRequiresPinnedPeer(t *testing.T) {
	var calls atomic.Int32
	e := agentsGateway(t, &calls)
	pinned := e.pin("desk1")

	rows, err := NewClient(pinned, peerOf(e)).Agents(context.Background())
	if err != nil || len(rows) != 2 || rows[0].AgentID != "aaaaaaaaaaaaaaaa" || rows[1].Note != NativeNote {
		t.Fatalf("pinned peer: rows %+v err %v", rows, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("source called %d times, want 1", calls.Load())
	}

	stranger, err := LoadOrCreateHost(newDir(t))
	if err != nil {
		t.Fatal(err)
	}
	before := calls.Load()
	if rows, err := NewClient(stranger, peerOf(e)).Agents(context.Background()); err == nil || len(rows) != 0 {
		t.Fatalf("unpinned client got rows %+v err %v", rows, err)
	}
	if calls.Load() != before {
		t.Fatal("source ran for an unpinned client")
	}

	hc := e.senderClient(pinned)
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		req, _ := http.NewRequest(m, "https://"+e.addr()+"/v1/agents", nil)
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s /v1/agents: status %d, want 405", m, resp.StatusCode)
		}
	}
	for _, p := range []string{"/v1/agents/x", "/v1/other", "/"} {
		resp, err := hc.Get("https://" + e.addr() + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: status %d, want 404", p, resp.StatusCode)
		}
	}

	resp, err := hc.Get("https://" + e.addr() + "/v1/agents")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("open connection before unpair: %v %v", resp, err)
	}
	resp.Body.Close()
	if err := e.peers.Remove("desk1"); err != nil {
		t.Fatal(err)
	}
	before = calls.Load()
	resp, err = hc.Get("https://" + e.addr() + "/v1/agents")
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("after unpair: status %d, want 403", resp.StatusCode)
		}
	}
	if calls.Load() != before {
		t.Fatal("source ran after unpair")
	}
}

func stallServer(t *testing.T) (Host, string) {
	t.Helper()
	h, err := LoadOrCreateHost(newDir(t))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{h.Certificate}, MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAnyClientCert,
	})
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				c.(*tls.Conn).Handshake()
				<-stop
				c.Close()
			}()
		}
	}()
	t.Cleanup(func() { close(stop); ln.Close() })
	return h, ln.Addr().String()
}

func TestClientErrorReasons(t *testing.T) {
	self, err := LoadOrCreateHost(newDir(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := func(d time.Duration) context.Context {
		c, cancel := context.WithTimeout(context.Background(), d)
		t.Cleanup(cancel)
		return c
	}
	reason := func(t *testing.T, err error) string {
		t.Helper()
		var re *Error
		if !errors.As(err, &re) {
			t.Fatalf("error %v is not a *remote.Error", err)
		}
		return re.Reason
	}

	t.Run("refused", func(t *testing.T) {
		ln, _ := net.Listen("tcp", "127.0.0.1:0")
		addr := ln.Addr().String()
		ln.Close()
		_, err := NewClient(self, Peer{Alias: "p", Address: addr, Fingerprint: self.Fingerprint}).Agents(ctx(2 * time.Second))
		if got := reason(t, err); got != ReasonGatewayNotRunning {
			t.Fatalf("reason %q (%v), want %q", got, err, ReasonGatewayNotRunning)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		h, addr := stallServer(t)
		start := time.Now()
		_, err := NewClient(self, Peer{Alias: "p", Address: addr, Fingerprint: h.Fingerprint}).Agents(ctx(300 * time.Millisecond))
		if got := reason(t, err); got != ReasonTimeout {
			t.Fatalf("reason %q (%v), want %q", got, err, ReasonTimeout)
		}
		if d := time.Since(start); d > time.Second {
			t.Fatalf("took %v for a 300ms deadline", d)
		}
	})
	t.Run("wrong pinned fingerprint", func(t *testing.T) {
		e := agentsGateway(t, nil)
		e.pin("me")
		p := peerOf(e)
		p.Fingerprint = self.Fingerprint
		_, err := NewClient(self, p).Agents(ctx(2 * time.Second))
		if got := reason(t, err); got != ReasonPinMismatch {
			t.Fatalf("reason %q (%v), want %q", got, err, ReasonPinMismatch)
		}
	})
	t.Run("peer unpairs us", func(t *testing.T) {
		e := agentsGateway(t, nil)
		_, err := NewClient(self, peerOf(e)).Agents(ctx(2 * time.Second))
		if got := reason(t, err); got != ReasonUnpairedByPeer {
			t.Fatalf("reason %q (%v), want %q", got, err, ReasonUnpairedByPeer)
		}
	})
	t.Run("other", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ln.Close() })
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				c.Write([]byte("not tls at all\r\n"))
				c.Close()
			}
		}()
		_, err = NewClient(self, Peer{Alias: "p", Address: ln.Addr().String(), Fingerprint: self.Fingerprint}).Agents(ctx(2 * time.Second))
		if got := reason(t, err); got != ReasonUnreachable {
			t.Fatalf("reason %q (%v), want %q", got, err, ReasonUnreachable)
		}
	})
}

func TestClientRejectsBadListings(t *testing.T) {
	cases := map[string]string{
		"not json":      `<html>`,
		"agents absent": `{}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			e := startGateway(t, func(c *GatewayConfig) {
				c.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) })
			})
			h := e.pin("me")
			_, err := NewClient(h, peerOf(e)).Agents(context.Background())
			var re *Error
			if !errors.As(err, &re) || re.Reason != ReasonInvalidListing {
				t.Fatalf("err %v, want invalid_listing", err)
			}
		})
	}
	t.Run("bad rows dropped good kept", func(t *testing.T) {
		good := sampleRows[0]
		rows := []map[string]any{
			{"agent_id": good.AgentID, "name": good.Name, "harness": "claude", "crosstalk": true, "role": "primary", "reachable": true, "status": "idle", "messageable": true},
			{"agent_id": "NOT-HEX", "name": "x", "harness": "claude"},
			{"agent_id": "cccccccccccccccc", "name": "ctl\x07char", "harness": "claude"},
			{"agent_id": "dddddddddddddddd", "name": "n", "harness": "gemini"},
			{"agent_id": "eeeeeeeeeeeeeeee", "name": "n", "harness": "codex", "crosstalk": true, "role": "admin"},
		}
		body, _ := json.Marshal(map[string]any{"agents": rows})
		e := startGateway(t, func(c *GatewayConfig) {
			c.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(body) })
		})
		h := e.pin("me")
		got, err := NewClient(h, peerOf(e)).Agents(context.Background())
		if err != nil || len(got) != 1 || got[0].AgentID != good.AgentID {
			t.Fatalf("rows %+v err %v, want only the valid row", got, err)
		}
	})
	t.Run("too many rows", func(t *testing.T) {
		rows := make([]AgentRow, 1001)
		for i := range rows {
			rows[i] = sampleRows[0]
		}
		body, _ := json.Marshal(map[string]any{"agents": rows})
		e := startGateway(t, func(c *GatewayConfig) {
			c.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(body) })
		})
		h := e.pin("me")
		_, err := NewClient(h, peerOf(e)).Agents(context.Background())
		var re *Error
		if !errors.As(err, &re) || re.Reason != ReasonInvalidListing {
			t.Fatalf("err %v, want invalid_listing", err)
		}
	})
	t.Run("body over 1 MiB", func(t *testing.T) {
		e := startGateway(t, func(c *GatewayConfig) {
			c.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(`{"agents":[],"pad":"`))
				w.Write(make([]byte, MaxResponse+10))
			})
		})
		h := e.pin("me")
		_, err := NewClient(h, peerOf(e)).Agents(context.Background())
		var re *Error
		if !errors.As(err, &re) || re.Reason != ReasonUnreachable {
			t.Fatalf("err %v, want unreachable", err)
		}
	})
}
