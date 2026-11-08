package shim

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/remote"
)

type rhost struct {
	t     *testing.T
	w     *world
	cfg   string
	host  remote.Host
	peers *remote.Peers
	n     *node
	gw    *remote.Gateway
	hits  atomic.Int32
	stop  context.CancelFunc
}

// newRHost is one independent host: its own state dir, config dir and
// harness fakes. A claude session called name is bound on it.
func newRHost(t *testing.T, pid int, name string) *rhost {
	t.Helper()
	h := &rhost{t: t, w: newWorld(t), cfg: filepath.Join(t.TempDir(), "crosstalk")}
	var err error
	if h.host, err = remote.LoadOrCreateHost(h.cfg); err != nil {
		t.Fatal(err)
	}
	h.peers = remote.NewPeers(h.cfg)
	if name != "" {
		h.w.claude(pid, "sess-"+name, name)
	}
	h.n = h.w.node(pid, nil, func(o *Options) { o.RemoteConfigDir = h.cfg })
	if name != "" {
		h.n.ok(nil, "receive_message", map[string]any{})
	}
	return h
}

func (h *rhost) serve() {
	h.t.Helper()
	fw := remote.NewForwarder(h.w.state, func(string) (string, bool) { return "", false })
	mux := remote.NewMux(fw, RemoteSource(h.n.srv))
	h.gw = remote.NewGateway(remote.GatewayConfig{
		StateDir: h.w.state, Host: h.host, Bind: "127.0.0.1", Port: 0, Peers: h.peers, Limits: remote.DefaultLimits(),
		Log: func(string, ...any) {},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h.hits.Add(1)
			mux.ServeHTTP(w, r)
		}),
	})
	ctx, cancel := context.WithCancel(context.Background())
	h.stop = cancel
	done := make(chan error, 1)
	go func() { done <- h.gw.Serve(ctx) }()
	select {
	case <-h.gw.Ready():
	case err := <-done:
		h.t.Fatalf("gateway: %v", err)
	case <-time.After(5 * time.Second):
		h.t.Fatal("gateway not ready")
	}
	h.t.Cleanup(func() { cancel(); <-done })
}

func (h *rhost) addr() string { return h.gw.Addr().String() }

// pair makes a and b know each other; a sees b as alias bAlias.
func pair(t *testing.T, a *rhost, aliasOfB string, b *rhost, aliasOfA string) {
	t.Helper()
	if err := a.peers.Upsert(remote.Peer{Alias: aliasOfB, Address: b.addr(), Fingerprint: b.host.Fingerprint}); err != nil {
		t.Fatal(err)
	}
	if err := b.peers.Upsert(remote.Peer{Alias: aliasOfA, Address: "192.0.2.1:7745", Fingerprint: a.host.Fingerprint}); err != nil {
		t.Fatal(err)
	}
}

func asRows(t *testing.T, out map[string]any) []map[string]any {
	t.Helper()
	var rows []map[string]any
	for _, r := range out["agents"].([]any) {
		rows = append(rows, r.(map[string]any))
	}
	return rows
}

func hostEntry(t *testing.T, out map[string]any, alias string) map[string]any {
	t.Helper()
	hs, _ := out["hosts"].([]any)
	for _, h := range hs {
		if m := h.(map[string]any); m["host"] == alias {
			return m
		}
	}
	return nil
}

var forbiddenRemoteKeys = []string{"cwd", "pid", "session_ref", "endpoint", "owner", "path"}

func checkNoLeak(t *testing.T, row map[string]any) {
	t.Helper()
	for _, k := range forbiddenRemoteKeys {
		if _, ok := row[k]; ok {
			t.Errorf("remote row %v has key %q", row, k)
		}
	}
}

func remoteRowsOf(rows []map[string]any, alias string) []map[string]any {
	var out []map[string]any
	for _, r := range rows {
		if r["host"] == alias {
			out = append(out, r)
		}
	}
	return out
}

func TestListAgentsShowsPairedHostAgents(t *testing.T) {
	h1 := newRHost(t, 4001, "alice")
	h2 := newRHost(t, 4001, "bob")
	h1.serve()
	h2.serve()
	pair(t, h1, "lab", h2, "desk")

	local2 := h2.n.ok(nil, "list_agents", map[string]any{})
	out := h1.n.ok(nil, "list_agents", map[string]any{})
	rows := asRows(t, out)

	var bob map[string]any
	for _, r := range rows {
		if r["name"] == "bob@lab" {
			bob = r
		}
	}
	if bob == nil {
		t.Fatalf("no bob@lab in %v", rows)
	}
	wantID := h2.w.claudeAgentID("sess-bob")
	if bob["host"] != "lab" || bob["harness"] != "claude" || bob["agent_id"] != wantID || bob["crosstalk"] != true || bob["messageable"] != true {
		t.Errorf("bob row %v", bob)
	}
	var bobLocal map[string]any
	for _, r := range asRows(t, local2) {
		if r["agent_id"] == wantID {
			bobLocal = r
		}
	}
	if bobLocal == nil || bob["reachable"] != bobLocal["reachable"] || bob["status"] != bobLocal["status"] {
		t.Errorf("remote reachable/status %v/%v differ from host 2's own %v", bob["reachable"], bob["status"], bobLocal)
	}
	rr := remoteRowsOf(rows, "lab")
	for _, r := range rr {
		checkNoLeak(t, r)
		b, _ := json.Marshal(r)
		if strings.Contains(string(b), "/work/") {
			t.Errorf("remote row leaks a path: %s", b)
		}
	}
	for _, r := range rows {
		if _, ok := r["host"]; ok && r["host"] != "lab" {
			t.Errorf("unexpected host on %v", r)
		}
		if r["name"] == "alice" {
			if _, ok := r["host"]; ok {
				t.Errorf("local row gained host: %v", r)
			}
		}
	}
	he := hostEntry(t, out, "lab")
	if he == nil || he["reachable"] != true || int(he["agents"].(float64)) != len(asRows(t, local2)) || len(rr) != len(asRows(t, local2)) {
		t.Errorf("hosts entry %v, remote rows %d, host 2 local count %d", he, len(rr), len(asRows(t, local2)))
	}
	if _, has := he["error"]; has {
		t.Errorf("hosts entry has error: %v", he)
	}
	if _, has := he["warning"]; has {
		t.Errorf("warning although this host's gateway runs: %v", he)
	}
}

func TestListAgentsGatewayDown(t *testing.T) {
	h1 := newRHost(t, 4001, "alice")
	h2 := newRHost(t, 4001, "bob")
	h2.serve()
	pair(t, h1, "lab", h2, "desk")
	h2.stop()
	waitFor(t, "gateway 2 stopped", func() bool {
		c, err := net.DialTimeout("tcp", h2.addr(), 200*time.Millisecond)
		if err == nil {
			c.Close()
		}
		return err != nil
	})

	start := time.Now()
	out := h1.n.ok(nil, "list_agents", map[string]any{})
	if d := time.Since(start); d > 1600*time.Millisecond {
		t.Fatalf("list_agents took %v with a refused peer", d)
	}
	he := hostEntry(t, out, "lab")
	if he == nil || he["reachable"] != false || he["error"] != "gateway_not_running" {
		t.Fatalf("hosts entry %v", he)
	}
	if w, _ := he["warning"].(string); w == "" {
		t.Errorf("no warning that this host cannot receive: %v", he)
	}
	found := false
	for _, r := range asRows(t, out) {
		if r["name"] == "alice" {
			found = true
		}
		if r["host"] == "lab" {
			t.Errorf("remote row from a down host: %v", r)
		}
	}
	if !found {
		t.Errorf("local rows missing: %v", out["agents"])
	}
}

// stallPeer accepts the TLS handshake and never answers a request.
func stallPeer(t *testing.T) remote.Peer {
	t.Helper()
	h, err := remote.LoadOrCreateHost(filepath.Join(t.TempDir(), "crosstalk"))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{h.Certificate}, MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAnyClientCert})
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
			go func() { c.(*tls.Conn).Handshake(); <-stop; c.Close() }()
		}
	}()
	t.Cleanup(func() { close(stop); ln.Close() })
	return remote.Peer{Address: ln.Addr().String(), Fingerprint: h.Fingerprint}
}

func TestListAgentsTimeoutBound(t *testing.T) {
	for _, n := range []int{1, 2} {
		t.Run(string(rune('0'+n))+" stalled", func(t *testing.T) {
			h1 := newRHost(t, 4001, "alice")
			for i := 0; i < n; i++ {
				p := stallPeer(t)
				p.Alias = "slow" + string(rune('a'+i))
				if err := h1.peers.Upsert(p); err != nil {
					t.Fatal(err)
				}
			}
			before := runtime.NumGoroutine()
			start := time.Now()
			out := h1.n.ok(nil, "list_agents", map[string]any{})
			if d := time.Since(start); d > 1600*time.Millisecond {
				t.Fatalf("list_agents took %v with %d stalled peers, want at most 1.5 s", d, n)
			}
			for i := 0; i < n; i++ {
				he := hostEntry(t, out, "slow"+string(rune('a'+i)))
				if he == nil || he["reachable"] != false || he["error"] != "timeout" {
					t.Errorf("hosts entry %v", he)
				}
			}
			found := false
			for _, r := range asRows(t, out) {
				found = found || r["name"] == "alice"
			}
			if !found {
				t.Error("local rows missing")
			}
			deadline := time.Now().Add(3 * time.Second)
			for runtime.NumGoroutine() > before+2 && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
			}
			if g := runtime.NumGoroutine(); g > before+2 {
				t.Errorf("goroutines %d after the call, %d before: fetch goroutines leaked", g, before)
			}
		})
	}
}

func TestRemoteNativeListedUnmessageable(t *testing.T) {
	h1 := newRHost(t, 4001, "alice")
	h2 := newRHost(t, 4001, "bob")
	h2.w.claude(4002, "sess-native", "native")
	h1.serve()
	h2.serve()
	pair(t, h1, "lab", h2, "desk")

	rows := asRows(t, h1.n.ok(nil, "list_agents", map[string]any{}))
	var nat map[string]any
	for _, r := range rows {
		if r["name"] == "native@lab" {
			nat = r
		}
	}
	if nat == nil {
		t.Fatalf("native session not listed: %v", rows)
	}
	if nat["crosstalk"] != false || nat["messageable"] != false || nat["note"] != remote.NativeNote || nat["host"] != "lab" {
		t.Errorf("native row %v", nat)
	}
	if _, has := nat["role"]; has {
		t.Errorf("native row carries a role: %v", nat)
	}
	checkNoLeak(t, nat)
}

func TestUnpairedHostNotListed(t *testing.T) {
	h1 := newRHost(t, 4001, "alice")
	h2 := newRHost(t, 4001, "bob")
	h1.serve()
	h2.serve()
	pair(t, h1, "lab", h2, "desk")

	out := h1.n.ok(nil, "list_agents", map[string]any{})
	if len(remoteRowsOf(asRows(t, out), "lab")) == 0 || h2.hits.Load() == 0 {
		t.Fatalf("paired host not listed (hits %d): %v", h2.hits.Load(), out)
	}
	if err := h1.peers.Remove("lab"); err != nil {
		t.Fatal(err)
	}
	hits := h2.hits.Load()
	out = h1.n.ok(nil, "list_agents", map[string]any{})
	if rr := remoteRowsOf(asRows(t, out), "lab"); len(rr) != 0 {
		t.Errorf("rows from an unpaired host: %v", rr)
	}
	if he := hostEntry(t, out, "lab"); he != nil {
		t.Errorf("hosts entry for an unpaired host: %v", he)
	}
	if h2.hits.Load() != hits {
		t.Errorf("%d requests reached the unpaired host", h2.hits.Load()-hits)
	}
}

func TestAgentsRowsAreAllowlisted(t *testing.T) {
	h1 := newRHost(t, 4001, "alice")
	h2 := newRHost(t, 4001, "bob")
	h2.w.claude(4002, "sess-native", "native")
	h2.serve()
	pair(t, h1, "lab", h2, "desk")

	status, body, err := remote.NewClient(h1.host, remote.Peer{Alias: "lab", Address: h2.addr(), Fingerprint: h2.host.Fingerprint}).Do(context.Background(), http.MethodGet, "/v1/agents", nil, nil)
	if err != nil || status != 200 {
		t.Fatalf("status %d err %v", status, err)
	}
	for _, leak := range []string{"/work/", "sess-bob", "sess-native", "\"pid\"", ".sock", "http://"} {
		if strings.Contains(string(body), leak) {
			t.Errorf("body contains %q: %s", leak, body)
		}
	}
	var got struct {
		Agents []map[string]any `json:"agents"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Agents) != 2 {
		t.Fatalf("rows %v", got.Agents)
	}
	base := []string{"agent_id", "crosstalk", "harness", "messageable", "name", "reachable", "status"}
	for _, r := range got.Agents {
		want := append([]string(nil), base...)
		if r["crosstalk"] == true {
			want = append(want, "role")
		} else {
			want = append(want, "note")
		}
		if len(r) != len(want) {
			t.Errorf("row keys %v, want exactly %v", keysOf(r), want)
		}
		for _, k := range want {
			if _, ok := r[k]; !ok {
				t.Errorf("row %v lacks %q", r, k)
			}
		}
	}
}

func keysOf(m map[string]any) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

func TestRemoteUnnamedNativeStaysVisible(t *testing.T) {
	h1 := newRHost(t, 4001, "alice")
	h2 := newRHost(t, 4001, "")
	h2.w.codex.addThread("T-anon", "")
	h2.serve()
	pair(t, h1, "lab", h2, "desk")

	out := h1.n.ok(nil, "list_agents", map[string]any{})
	rr := remoteRowsOf(asRows(t, out), "lab")
	if len(rr) != 1 || rr[0]["crosstalk"] != false || rr[0]["messageable"] != false || !strings.HasPrefix(rr[0]["name"].(string), "unnamed-") {
		t.Fatalf("unnamed native row missing or wrong: %v", rr)
	}
	if he := hostEntry(t, out, "lab"); he == nil || he["reachable"] != true || he["error"] != nil {
		t.Fatalf("host entry %v", he)
	}
}

func TestBrokenPeersFileWarnsAndKeepsLocalRows(t *testing.T) {
	cases := map[string]func(t *testing.T, h *rhost){
		"malformed json": func(t *testing.T, h *rhost) {
			if err := os.WriteFile(filepath.Join(h.cfg, "peers.json"), []byte("{not json"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"wrong mode": func(t *testing.T, h *rhost) {
			if err := os.WriteFile(filepath.Join(h.cfg, "peers.json"), []byte(`{"version":1,"peers":[]}`), 0o644); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, break_ := range cases {
		t.Run(name, func(t *testing.T) {
			h1 := newRHost(t, 4001, "alice")
			break_(t, h1)
			out := h1.n.ok(nil, "list_agents", map[string]any{})
			ws, _ := out["warnings"].([]any)
			if len(ws) != 1 || !strings.Contains(ws[0].(string), "peers.json") {
				t.Fatalf("warnings %v, want one naming peers.json", out["warnings"])
			}
			if len(asRows(t, out)) != 1 || asRows(t, out)[0]["name"] != "alice" {
				t.Fatalf("local rows changed: %v", out["agents"])
			}
		})
	}
	t.Run("absent file is silent", func(t *testing.T) {
		h1 := newRHost(t, 4001, "alice")
		out := h1.n.ok(nil, "list_agents", map[string]any{})
		if _, has := out["warnings"]; has {
			t.Fatalf("warnings %v for a host with no peers", out["warnings"])
		}
	})
}

func TestUnnamedFallbackShortID(t *testing.T) {
	for in, want := range map[string]string{"": "unnamed-", "ab": "unnamed-ab", "0123456789abcdef": "unnamed-012345"} {
		if got := unnamedFallback(in); got != want {
			t.Errorf("unnamedFallback(%q) = %q, want %q", in, got, want)
		}
	}
}
