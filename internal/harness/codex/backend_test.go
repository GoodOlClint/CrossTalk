package codex_test

import (
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/harness/codex"
	"github.com/GoodOlClint/crosstalk/internal/harness/codex/appserver"
	"github.com/GoodOlClint/crosstalk/internal/harness/codex/appserver/appservertest"
)

func backendOf(t *testing.T, a *codex.Adapter) string {
	t.Helper()
	ts := a.LoadedThreads(ctx(t))
	if len(ts) != 1 {
		t.Fatalf("LoadedThreads = %+v", ts)
	}
	return ts[0].Backend
}

func TestBackendFromRateLimits(t *testing.T) {
	for _, c := range []struct {
		name    string
		setup   func(*appservertest.Server)
		want    string
		probing bool
	}{
		{"success", func(*appservertest.Server) {}, "reachable", true},
		{"outage", func(s *appservertest.Server) { s.RateLimitsErr = appservertest.BackendDownError() }, "unreachable", true},
		{"other error", func(s *appservertest.Server) {
			s.RateLimitsErr = &appserver.RPCError{Code: -32600, Message: "not supported"}
		}, "unknown", true},
		{"api key account", func(s *appservertest.Server) {
			s.Account = "apiKey"
			s.RateLimitsErr = appservertest.BackendDownError()
		}, "unknown", false},
		{"no account", func(s *appservertest.Server) { s.Account = "none"; s.RateLimitsErr = appservertest.BackendDownError() }, "unknown", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t, idleThread())
			c.setup(r.srv)
			if got := backendOf(t, r.a); got != c.want {
				t.Errorf("Backend = %q, want %q", got, c.want)
			}
			if n := len(r.srv.CallsTo("account/rateLimits/read")); (n > 0) != c.probing {
				t.Errorf("rateLimits calls = %d, want probing=%v", n, c.probing)
			}
		})
	}
}

func TestBackendOtherProviderIsUnknown(t *testing.T) {
	th := idleThread()
	th.ModelProvider = "custom"
	r := newRig(t, th)
	r.srv.RateLimitsErr = appservertest.BackendDownError()
	if got := backendOf(t, r.a); got != "unknown" {
		t.Errorf("Backend = %q, want unknown", got)
	}
}

func TestBackendHangIsUnknownWithinTimeout(t *testing.T) {
	old := codex.BackendProbeTimeout
	codex.BackendProbeTimeout = 150 * time.Millisecond
	t.Cleanup(func() { codex.BackendProbeTimeout = old })
	r := newRig(t, idleThread())
	r.srv.RateLimitsHang = true
	start := time.Now()
	ts := r.a.LoadedThreads(ctx(t))
	if len(ts) != 1 || ts[0].Backend != "unknown" || !ts[0].Session.Reachable {
		t.Fatalf("LoadedThreads = %+v", ts)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("listing took %v", d)
	}
}

func TestBackendCache(t *testing.T) {
	old := codex.BackendTTL
	codex.BackendTTL = 200 * time.Millisecond
	t.Cleanup(func() { codex.BackendTTL = old })
	r := newRig(t, idleThread())
	r.srv.RateLimitsErr = appservertest.BackendDownError()
	ep := r.srv.Endpoint()
	for range 2 {
		if got := r.a.Backend(ctx(t), ep); got != "unreachable" {
			t.Fatalf("Backend = %q", got)
		}
	}
	if n := len(r.srv.CallsTo("account/rateLimits/read")); n != 1 {
		t.Fatalf("rateLimits calls within TTL = %d, want 1", n)
	}
	if got := backendOf(t, r.a); got != "unreachable" {
		t.Fatalf("listing Backend = %q", got)
	}
	if n := len(r.srv.CallsTo("account/rateLimits/read")); n != 1 {
		t.Fatalf("listing inside TTL probed again: %d", n)
	}
	time.Sleep(300 * time.Millisecond)
	r.a.Backend(ctx(t), ep)
	if n := len(r.srv.CallsTo("account/rateLimits/read")); n != 2 {
		t.Fatalf("rateLimits calls after TTL = %d, want 2", n)
	}
}

func TestBackendUnreachableEndpointIsUnknown(t *testing.T) {
	a := &codex.Adapter{ControlSocket: t.TempDir() + "/absent.sock"}
	if got := a.Backend(ctx(t), "unix://"+t.TempDir()+"/absent.sock"); got != "unknown" {
		t.Errorf("Backend = %q", got)
	}
}
