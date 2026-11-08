package wire

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/identity"
	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/receiver"
	"github.com/GoodOlClint/crosstalk/internal/registry"
	"github.com/GoodOlClint/crosstalk/internal/remote"
	"github.com/GoodOlClint/crosstalk/internal/shim"
)

// rhost is one independent host: its own state dir, config dir, agents and
// gateway. Agents are Codex threads served by shims over the wire package.
type rhost struct {
	t     *testing.T
	state string
	cfg   string
	host  remote.Host
	peers *remote.Peers
	nodes []*node
	gw    *remote.Gateway
	stop  context.CancelFunc

	mu   sync.Mutex
	reqs []string
	// hook, when set, sees every request first; true means it answered.
	hook func(w http.ResponseWriter, r *http.Request, next http.Handler) bool
}

func newRHost(t *testing.T) *rhost {
	t.Helper()
	h := &rhost{t: t, state: shortDir(t), cfg: filepath.Join(shortDir(t), "crosstalk")}
	var err error
	if h.host, err = remote.LoadOrCreateHost(h.cfg); err != nil {
		t.Fatal(err)
	}
	h.peers = remote.NewPeers(h.cfg)
	return h
}

var rhostPPID = 100

// agent registers a Codex thread named name on h; extra names unserved
// (native) threads visible to the same harness.
func (h *rhost) agent(name string, extra ...string) *node {
	h.t.Helper()
	rhostPPID++
	n := newNodeIn(h.t, h.state, h.cfg, rhostPPID, name, "T-"+name+fmt.Sprint(rhostPPID), extra...)
	n.ok("receive_message", map[string]any{})
	h.nodes = append(h.nodes, n)
	return n
}

func (h *rhost) registeredURL(agentID string) (string, bool) {
	ds, err := registry.New(h.state).List()
	if err != nil {
		return "", false
	}
	for _, d := range ds {
		if d.AgentID == agentID && d.A2AURL != "" {
			return d.A2AURL, true
		}
	}
	return "", false
}

func (h *rhost) serve() {
	h.t.Helper()
	fw := remote.NewForwarder(h.state, h.registeredURL)
	mux := remote.NewMux(fw, shim.RemoteSource(h.nodes[0].srv))
	h.gw = remote.NewGateway(remote.GatewayConfig{
		StateDir: h.state, Host: h.host, Bind: "127.0.0.1", Port: 0, Peers: h.peers, Limits: testLimits(),
		Log: func(string, ...any) {},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h.mu.Lock()
			h.reqs = append(h.reqs, r.Method+" "+r.URL.Path)
			hook := h.hook
			h.mu.Unlock()
			if hook != nil && hook(w, r, mux) {
				return
			}
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

func (h *rhost) log() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.reqs...)
}

func (h *rhost) count(prefix string) int {
	n := 0
	for _, r := range h.log() {
		if strings.Contains(r, prefix) {
			n++
		}
	}
	return n
}

func (h *rhost) setHook(f func(w http.ResponseWriter, r *http.Request, next http.Handler) bool) {
	h.mu.Lock()
	h.hook = f
	h.mu.Unlock()
}

// pairHosts makes a see b as aliasOfB and b see a as aliasOfA.
func pairHosts(t *testing.T, a *rhost, aliasOfB string, b *rhost, aliasOfA string) {
	t.Helper()
	if err := a.peers.Upsert(remote.Peer{Alias: aliasOfB, Address: b.addr(), Fingerprint: b.host.Fingerprint}); err != nil {
		t.Fatal(err)
	}
	if err := b.peers.Upsert(remote.Peer{Alias: aliasOfA, Address: a.addr(), Fingerprint: a.host.Fingerprint}); err != nil {
		t.Fatal(err)
	}
}

// twoHosts is host 1 with alice and host 2 with bob, paired as lab and desk.
func twoHosts(t *testing.T, bobExtra ...string) (h1, h2 *rhost, alice, bob *node) {
	t.Helper()
	h1, h2 = newRHost(t), newRHost(t)
	alice, bob = h1.agent("alice"), h2.agent("bob", bobExtra...)
	h1.serve()
	h2.serve()
	pairHosts(t, h1, "lab", h2, "desk")
	return
}

func str(m map[string]any, k string) string { s, _ := m[k].(string); return s }

func journalOf(t *testing.T, state, agentID string) []journal.Record {
	t.Helper()
	b, err := os.ReadFile(journal.Path(state, agentID))
	if err != nil {
		t.Fatal(err)
	}
	var rs []journal.Record
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var r journal.Record
		if json.Unmarshal([]byte(l), &r) == nil {
			rs = append(rs, r)
		}
	}
	return rs
}

func texts(c []adapter.Call) []string {
	var out []string
	for _, w := range c {
		out = append(out, w.Message.Parts[0].Text)
	}
	return out
}

func TestBareNameNeverLeavesHost(t *testing.T) {
	h1, h2, alice, bob := twoHosts(t)
	_ = h1
	out := alice.ok("send_message", map[string]any{"to": "bob", "text": "hi"})
	if str(out, "reason") != "no_such_agent" {
		t.Fatalf("send to a bare name that exists only on host 2: %v", out)
	}
	if !strings.Contains(str(out, "hint"), "bob@lab") {
		t.Errorf("hint %q does not name bob@lab", str(out, "hint"))
	}
	if n := h2.count("/v1/a2a/"); n != 0 {
		t.Errorf("host 2's gateway saw %d forwarded requests for a bare name: %v", n, h2.log())
	}
	if len(bob.codex.Writes()) != 0 {
		t.Errorf("host 2 injected %v", texts(bob.codex.Writes()))
	}
	out = alice.ok("ask", map[string]any{"to": "bob", "text": "hi", "deadline_seconds": 5})
	if str(out, "reason") != "no_such_agent" || h2.count("/v1/a2a/") != 0 {
		t.Errorf("ask to a bare name: %v, forwarded %d", out, h2.count("/v1/a2a/"))
	}
}

func TestTwoHostAsk(t *testing.T) {
	h1, h2, alice, bob := twoHosts(t)
	out := alice.ok("ask", map[string]any{"to": "bob@lab", "text": "question", "deadline_seconds": 2})
	if str(out, "state") != "accepted" {
		t.Fatalf("ask: %v", out)
	}
	askID := str(out, "message_id")
	want := receiver.RemoteKey(h1.host.Fingerprint, alice.id())
	var routed bool
	for _, r := range journalOf(t, h2.state, bob.id()) {
		if r.Type == journal.TypeRoute && r.Sender == want {
			routed = true
		}
	}
	if !routed {
		t.Fatalf("host 2 opened no route keyed %s", want)
	}
	h2.stop()
	waitFor(t, "gateway 2 stopped", func() bool {
		c, err := net.DialTimeout("tcp", h2.addr(), 200*time.Millisecond)
		if err == nil {
			c.Close()
		}
		return err != nil
	})
	waitFor(t, "expiry notice on host 1", func() bool {
		for _, s := range texts(alice.codex.Writes()) {
			if strings.Contains(s, "Your ask "+askID) && strings.Contains(s, "expired after 2 s") {
				return true
			}
		}
		return false
	})
	if n := len(bob.codex.Writes()); n != 1 {
		t.Errorf("host 2 injected %d times, want 1", n)
	}
}

func TestUnknownAliasRejected(t *testing.T) {
	_, h2, alice, bob := twoHosts(t)
	for _, to := range []string{"bob@nope", bob.id() + "@nope"} {
		for _, tool := range []string{"send_message", "ask"} {
			args := map[string]any{"to": to, "text": "hi"}
			if tool == "ask" {
				args["deadline_seconds"] = 5
			}
			out := alice.ok(tool, args)
			if str(out, "state") != "rejected" || str(out, "reason") != "unknown_alias" {
				t.Errorf("%s to %s: %v", tool, to, out)
			}
		}
	}
	if n := len(h2.log()); n != 0 {
		t.Errorf("host 2 saw %d requests for an unknown alias: %v", n, h2.log())
	}
}

func TestLocalNameWithAtStaysLocal(t *testing.T) {
	h1, h2, alice, bob := twoHosts(t)
	build := h1.agent("build@desktop")
	_ = bob
	// "desktop" is a paired alias of host 2.
	if err := h1.peers.Upsert(remote.Peer{Alias: "desktop", Address: h2.addr(), Fingerprint: h2.host.Fingerprint}); err != nil {
		t.Fatal(err)
	}
	out := alice.ok("send_message", map[string]any{"to": "build@desktop", "text": "local please"})
	if str(out, "state") != "accepted" {
		t.Fatalf("local build@desktop: %v", out)
	}
	if len(build.codex.Writes()) != 1 {
		t.Fatalf("local build injected %d times", len(build.codex.Writes()))
	}
	if n := h2.count("/v1/a2a/"); n != 0 {
		t.Errorf("%d messages went to host 2: %v", n, h2.log())
	}

	remoteBuild := h2.agent("build")
	out = alice.ok("send_message", map[string]any{"to": "build@desktop", "text": "which one"})
	if str(out, "reason") != "ambiguous_name" || str(out, "state") != "rejected" {
		t.Fatalf("local and remote build@desktop: %v", out)
	}
	hint := str(out, "hint")
	if !strings.Contains(hint, build.id()) || !strings.Contains(hint, remoteBuild.id()+"@desktop") {
		t.Errorf("hint %q lacks the local agent_id %s and %s@desktop", hint, build.id(), remoteBuild.id())
	}
	if n := h2.count("/v1/a2a/"); n != 0 || len(build.codex.Writes()) != 1 || len(remoteBuild.codex.Writes()) != 0 {
		t.Errorf("an ambiguous send delivered: forwarded %d, local %d, remote %d", n, len(build.codex.Writes()), len(remoteBuild.codex.Writes()))
	}

	for _, tool := range []string{"claim", "release"} {
		res, err := alice.cs.CallTool(context.Background(), &mcp.CallToolParams{Meta: map[string]any{"threadId": alice.thread}, Name: tool, Arguments: map[string]any{"name": "build@desktop"}})
		b, _ := json.Marshal(res)
		if err != nil || !res.IsError || !strings.Contains(string(b), "ambiguous_name") {
			t.Errorf("%s with a collision: %v %s", tool, err, b)
		}
	}
	ds, _ := registry.New(h1.state).List()
	for _, d := range ds {
		if d.AgentID == build.id() && (d.Owner != nil || d.Role != registry.RolePrimary) {
			t.Errorf("local build's ownership changed: %+v", d)
		}
	}

	h2.stop()
	waitFor(t, "gateway 2 stopped", func() bool {
		c, err := net.DialTimeout("tcp", h2.addr(), 200*time.Millisecond)
		if err == nil {
			c.Close()
		}
		return err != nil
	})
	for _, tool := range []string{"send_message", "ask"} {
		args := map[string]any{"to": "build@desktop", "text": "peer is down"}
		if tool == "ask" {
			args["deadline_seconds"] = 5
		}
		out := alice.ok(tool, args)
		if st := str(out, "state"); st == "accepted" || st == "queued" {
			t.Errorf("%s with the peer's listing failing was delivered: %v", tool, out)
		}
		if !strings.Contains(str(out, "hint"), build.id()) {
			t.Errorf("hint %q does not offer the local agent_id", str(out, "hint"))
		}
	}
	if len(build.codex.Writes()) != 1 || len(remoteBuild.codex.Writes()) != 0 {
		t.Errorf("a send with an unanswered peer reached an agent: local %d, remote %d", len(build.codex.Writes()), len(remoteBuild.codex.Writes()))
	}
}

// padded answers r through next, then adds a field the decoder ignores but
// must read past, so the response is valid and over the cap.
func padded(w http.ResponseWriter, r *http.Request, next http.Handler) {
	rec := httptest.NewRecorder()
	next.ServeHTTP(rec, r)
	var doc map[string]any
	json.Unmarshal(rec.Body.Bytes(), &doc)
	doc["pad"] = strings.Repeat("x", remote.MaxResponse+10)
	b, _ := json.Marshal(doc)
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}

func TestRemoteResponseOverCapIsUnknownNotDelivered(t *testing.T) {
	t.Run("send", func(t *testing.T) {
		_, h2, alice, _ := twoHosts(t)
		h2.setHook(func(w http.ResponseWriter, r *http.Request, next http.Handler) bool {
			if r.Method != http.MethodPost {
				return false
			}
			padded(w, r, next)
			return true
		})
		out := alice.ok("send_message", map[string]any{"to": "bob@lab", "text": "hi"})
		if str(out, "state") != "unknown" || str(out, "reason") != "pending" {
			t.Errorf("%v", out)
		}
	})
	t.Run("poll", func(t *testing.T) {
		_, h2, alice, _ := twoHosts(t)
		var n atomic.Int32
		h2.setHook(func(w http.ResponseWriter, r *http.Request, next http.Handler) bool {
			if r.Method != http.MethodPost || n.Add(1) == 1 {
				return false
			}
			padded(w, r, next)
			return true
		})
		out := alice.ok("send_message", map[string]any{"to": "bob@lab", "text": "hi"})
		if str(out, "state") != "unknown" || str(out, "reason") != "pending" {
			t.Errorf("%v", out)
		}
	})
}

func TestRemoteNameWithSpacesAndUnicode(t *testing.T) {
	h1, h2, _, bob := twoHosts(t)
	names := []string{"CrossTalk Remote Plan Worker", "Zoë-日本語-ñ"}
	for _, name := range names {
		n := h1.agent(name)
		out := n.ok("send_message", map[string]any{"to": "bob@lab", "text": "from " + name})
		if str(out, "state") != "accepted" {
			t.Fatalf("%q: %v", name, out)
		}
	}
	ws := bob.codex.Writes()
	if len(ws) != len(names) {
		t.Fatalf("host 2 injected %d, want %d", len(ws), len(names))
	}
	for i, name := range names {
		if got, want := ws[i].Message.From, name+"@desk"; got != want || !ws[i].Delivery.VerifiedSender {
			t.Errorf("label %q verified %v, want %q byte for byte", got, ws[i].Delivery.VerifiedSender, want)
		}
	}
	_ = h2
}

func renameDescriptor(t *testing.T, state, agentID, name string) {
	path := registry.New(state).DescriptorPath(agentID)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Errorf("rename %s: %v", agentID, err)
		return
	}
	var d map[string]any
	json.Unmarshal(b, &d)
	d["name"] = name
	b, _ = json.Marshal(d)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Errorf("rename %s: %v", agentID, err)
	}
}

func TestRemoteNameReuseBetweenResolveAndPoll(t *testing.T) {
	h1, h2, alice, bob := twoHosts(t)
	_ = h1
	reuser := h2.agent("bob-later")
	var first atomic.Bool
	h2.setHook(func(w http.ResponseWriter, r *http.Request, next http.Handler) bool {
		if r.Method != http.MethodPost || !first.CompareAndSwap(false, true) {
			return false
		}
		rec := httptest.NewRecorder()
		next.ServeHTTP(rec, r)
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		w.Write(rec.Body.Bytes())
		renameDescriptor(t, h2.state, bob.id(), "bob-old")
		renameDescriptor(t, h2.state, reuser.id(), "bob")
		return true
	})
	out := alice.ok("send_message", map[string]any{"to": "bob@lab", "text": "hi"})
	if str(out, "state") != "accepted" {
		t.Fatalf("send: %v", out)
	}
	wantPath := "POST /v1/a2a/" + bob.id()
	posts := 0
	for _, r := range h2.log() {
		if strings.HasPrefix(r, "POST ") {
			posts++
			if r != wantPath {
				t.Errorf("request %q, want every POST on %s", r, wantPath)
			}
		}
	}
	if posts < 2 {
		t.Errorf("only %d POSTs: no poll happened: %v", posts, h2.log())
	}
	if n := h2.count("GET /v1/agents"); n != 1 {
		t.Errorf("host 2 listed %d times, want 1 (no fresh lookup during the poll)", n)
	}
	if len(bob.codex.Writes()) != 1 || len(reuser.codex.Writes()) != 0 {
		t.Errorf("injects: resolved agent %d, name reuser %d", len(bob.codex.Writes()), len(reuser.codex.Writes()))
	}
}

func TestRemoteNativeTargetRejected(t *testing.T) {
	_, h2, alice, bob := twoHosts(t, "natasha")
	nativeID := identity.AgentID("codex", "T-natasha")
	for _, to := range []string{"natasha@lab", nativeID + "@lab"} {
		for _, tool := range []string{"send_message", "ask"} {
			args := map[string]any{"to": to, "text": "hi"}
			if tool == "ask" {
				args["deadline_seconds"] = 5
			}
			out := alice.ok(tool, args)
			if str(out, "state") != "rejected" || str(out, "reason") != "remote_native_unsupported" || str(out, "hint") == "" {
				t.Errorf("%s to %s: %v", tool, to, out)
			}
		}
	}
	if n := h2.count("POST /v1/a2a/"); n != 0 {
		t.Errorf("%d forwarded requests for a native target", n)
	}
	if len(bob.codex.Writes()) != 0 {
		t.Errorf("host 2 injected %v", texts(bob.codex.Writes()))
	}
	out := alice.ok("send_message", map[string]any{"to": "nobody@lab", "text": "hi"})
	if str(out, "reason") != "no_such_agent" {
		t.Errorf("nobody@lab: %v", out)
	}
}

func TestNudgeCountsRemotePeer(t *testing.T) {
	nudged := func(out map[string]any) bool { return strings.Contains(str(out, "hint"), "You have sent this peer") }
	t.Run("30 to one agent", func(t *testing.T) {
		h1, h2, alice, _ := twoHosts(t)
		_, _ = h1, h2
		for i := 1; i <= 30; i++ {
			out := alice.ok("send_message", map[string]any{"to": "bob@lab", "text": fmt.Sprint("m", i)})
			if str(out, "state") != "accepted" {
				t.Fatalf("send %d: %v", i, out)
			}
			if got := nudged(out); got != (i == 30) {
				t.Fatalf("send %d: nudge %v, hint %q", i, got, str(out, "hint"))
			}
		}
	})
	t.Run("29 plus 1 across two agents", func(t *testing.T) {
		h1, h2, alice, _ := twoHosts(t)
		h2.agent("carol")
		_ = h1
		for i := 1; i <= 29; i++ {
			out := alice.ok("send_message", map[string]any{"to": "bob@lab", "text": fmt.Sprint("m", i)})
			if nudged(out) {
				t.Fatalf("send %d nudged", i)
			}
		}
		out := alice.ok("send_message", map[string]any{"to": "carol@lab", "text": "one"})
		if str(out, "state") != "accepted" || nudged(out) {
			t.Fatalf("the 30th send to the peer host, to another agent: %v", out)
		}
	})
}

func TestRemoteSendRefusesCommandClaimRelease(t *testing.T) {
	_, h2, alice, bob := twoHosts(t)
	out := alice.ok("send_message", map[string]any{"to": "bob@lab", "text": "do it", "kind": "command"})
	if str(out, "state") != "rejected" || str(out, "reason") != "remote_kind" {
		t.Errorf("command: %v", out)
	}
	for _, tool := range []string{"claim", "release"} {
		for _, to := range []string{"bob@lab", bob.id() + "@lab"} {
			res, err := alice.cs.CallTool(context.Background(), &mcp.CallToolParams{Meta: map[string]any{"threadId": alice.thread}, Name: tool, Arguments: map[string]any{"name": to}})
			b, _ := json.Marshal(res)
			if err != nil || !res.IsError || !strings.Contains(string(b), "remote_kind") {
				t.Errorf("%s %s: %v %s", tool, to, err, b)
			}
		}
	}
	if n := len(h2.log()); n != 0 {
		t.Errorf("%d network calls for refused kinds: %v", n, h2.log())
	}
}

func TestRemoteSendJournalsBeforeSend(t *testing.T) {
	h1, h2, alice, bob := twoHosts(t)
	gate := make(chan struct{})
	h2.setHook(func(w http.ResponseWriter, r *http.Request, next http.Handler) bool {
		if r.Method == http.MethodPost {
			<-gate
		}
		return false
	})
	done := make(chan map[string]any, 1)
	go func() {
		_, out := alice.call("send_message", map[string]any{"to": "bob@lab", "text": "hang"})
		done <- out
	}()
	want := receiver.RemoteKey(h2.host.Fingerprint, bob.id())
	_ = h1
	waitFor(t, "the POST to reach host 2", func() bool { return h2.count("POST /v1/a2a/") == 1 })
	var found bool
	for _, r := range journalOf(t, h1.state, alice.id()) {
		if r.Type == journal.TypeOutbound && r.To == want && r.State == journal.StateSubmitted {
			found = true
		}
	}
	close(gate)
	if !found {
		t.Errorf("no outbound record with to=%s while the POST was in flight", want)
	}
	if out := <-done; str(out, "state") != "accepted" {
		t.Errorf("send after release: %v", out)
	}
}

func TestRemoteSendStateMapping(t *testing.T) {
	send := func(alice *node) map[string]any {
		return alice.ok("send_message", map[string]any{"to": "bob@lab", "text": "hi"})
	}
	t.Run("accepted", func(t *testing.T) {
		_, _, alice, _ := twoHosts(t)
		if out := send(alice); str(out, "state") != "accepted" || str(out, "reason") != "" {
			t.Errorf("%v", out)
		}
	})
	t.Run("queued", func(t *testing.T) {
		_, _, alice, bob := twoHosts(t)
		bob.codex.SetDefaultInject(adapter.Outcome{State: adapter.Queued})
		if out := send(alice); str(out, "state") != "queued" {
			t.Errorf("%v", out)
		}
	})
	t.Run("unknown pending after the gateway forwarded", func(t *testing.T) {
		_, h2, alice, bob := twoHosts(t)
		h2.setHook(func(w http.ResponseWriter, r *http.Request, next http.Handler) bool {
			if r.Method != http.MethodPost {
				return false
			}
			next.ServeHTTP(httptest.NewRecorder(), r)
			if hj, ok := w.(http.Hijacker); ok {
				if c, _, err := hj.Hijack(); err == nil {
					c.Close()
				}
			}
			return true
		})
		out := send(alice)
		if str(out, "state") != "unknown" || str(out, "reason") != "pending" {
			t.Errorf("%v", out)
		}
		waitFor(t, "the forwarded message to be injected", func() bool { return len(bob.codex.Writes()) == 1 })
	})
	t.Run("rejected by the gateway", func(t *testing.T) {
		_, h2, alice, bob := twoHosts(t)
		h2.setHook(func(w http.ResponseWriter, r *http.Request, next http.Handler) bool {
			if r.Method == http.MethodPost {
				os.Remove(registry.New(h2.state).DescriptorPath(bob.id()))
			}
			return false
		})
		out := send(alice)
		if str(out, "state") != "rejected" || str(out, "reason") != "no_such_agent" {
			t.Errorf("%v", out)
		}
	})
	t.Run("unreachable gateway_not_running", func(t *testing.T) {
		_, h2, alice, _ := twoHosts(t)
		h2.stop()
		waitFor(t, "gateway stopped", func() bool {
			c, err := net.DialTimeout("tcp", h2.addr(), 200*time.Millisecond)
			if err == nil {
				c.Close()
			}
			return err != nil
		})
		out := send(alice)
		if str(out, "state") != "unreachable" || str(out, "reason") != "gateway_not_running" {
			t.Errorf("%v", out)
		}
	})
	t.Run("unreachable tls_pin_mismatch", func(t *testing.T) {
		h1, h2, alice, _ := twoHosts(t)
		h1.peers.Remove("lab")
		if err := h1.peers.Upsert(remote.Peer{Alias: "lab", Address: h2.addr(), Fingerprint: strings.Repeat("ab", 32)}); err != nil {
			t.Fatal(err)
		}
		out := send(alice)
		if str(out, "state") != "unreachable" || str(out, "reason") != "tls_pin_mismatch" {
			t.Errorf("%v", out)
		}
	})
	t.Run("rejected unpaired_by_peer", func(t *testing.T) {
		h1, h2, alice, _ := twoHosts(t)
		if err := h2.peers.Remove("desk"); err != nil {
			t.Fatal(err)
		}
		_ = h1
		out := send(alice)
		if str(out, "state") != "rejected" || str(out, "reason") != "unpaired_by_peer" {
			t.Errorf("%v", out)
		}
	})
	t.Run("unreachable timeout", func(t *testing.T) {
		h1, _, alice, _ := twoHosts(t)
		stalled := stallPeer(t)
		h1.peers.Remove("lab")
		if err := h1.peers.Upsert(remote.Peer{Alias: "lab", Address: stalled.Address, Fingerprint: stalled.Fingerprint}); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		out := send(alice)
		if str(out, "state") != "unreachable" || str(out, "reason") != "timeout" {
			t.Errorf("%v", out)
		}
		if d := time.Since(start); d > 3*time.Second {
			t.Errorf("took %v", d)
		}
	})
}

// stallPeer accepts the TLS handshake and never answers a request.
func stallPeer(t *testing.T) remote.Peer {
	t.Helper()
	h, err := remote.LoadOrCreateHost(filepath.Join(shortDir(t), "crosstalk"))
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

func testLimits() remote.Limits {
	l := remote.DefaultLimits()
	l.MaxPeerPerMinute = 100000
	return l
}
