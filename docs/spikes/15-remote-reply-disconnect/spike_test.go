// Package remotereply is throwaway spike S4 code: an ask and its reply across
// a gateway disconnect, between two state dirs on 127.0.0.1.
package remotereply

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/GoodOlClint/crosstalk/internal/a2a"
	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/harness"
	"github.com/GoodOlClint/crosstalk/internal/harness/codex"
	"github.com/GoodOlClint/crosstalk/internal/identity"
	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/mcpserver"
	"github.com/GoodOlClint/crosstalk/internal/msg"
	"github.com/GoodOlClint/crosstalk/internal/receiver"
	"github.com/GoodOlClint/crosstalk/internal/registry"
	"github.com/GoodOlClint/crosstalk/internal/shim"
	"github.com/GoodOlClint/crosstalk/internal/udsingress"
	"github.com/GoodOlClint/crosstalk/internal/wire"
)

const attestPrefix = "spike-attest."

func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "s4")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type fakeCodex struct {
	*adapter.Fake
	mu      sync.Mutex
	threads []codex.LoadedThread
}

func (f *fakeCodex) LoadedThreads(context.Context, ...string) []codex.LoadedThread {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]codex.LoadedThread(nil), f.threads...)
}

// host is one machine: a state dir, a gateway, and the production sender
// (wire.Wire.SendTask) its agents use to reach the other host.
type host struct {
	t        *testing.T
	fp       string // stand-in for the pinned mTLS fingerprint of THIS host
	state    string
	credPath string // the gateway credential its agents' A2A servers accept
	gw       *gateway
	w        *wire.Wire
	peerGW   func() string // URL of the paired host's gateway
}

func newHost(t *testing.T, fp string) *host {
	h := &host{t: t, fp: fp, state: shortDir(t)}
	h.credPath = filepath.Join(h.state, "gateway.cred")
	b := make([]byte, 16)
	rand.Read(b)
	if err := os.WriteFile(h.credPath, []byte(hex.EncodeToString(b)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := wire.New(shim.Options{StateDir: h.state, PPID: 1})
	if err != nil {
		t.Fatal(err)
	}
	h.w = w
	t.Cleanup(func() { w.Shim().Close() })
	return h
}

// attest writes the file wire.SendTask reads as the sender's "own token". It
// stands in for the headers production attests the sender with (R5).
func (h *host) attest(agentID string) string {
	dir := filepath.Join(h.state, "attest")
	os.MkdirAll(dir, 0o700)
	p := filepath.Join(dir, agentID)
	if err := os.WriteFile(p, []byte(attestPrefix+agentID+"\n"), 0o600); err != nil {
		h.t.Fatal(err)
	}
	return p
}

// send is the production send path to a remote agent through its gateway.
func (h *host) send(ctx context.Context, fromName, fromID, toGatewayURL, toID string, m msg.Message) (string, adapter.State, error) {
	from := registry.Descriptor{Name: fromName, AgentID: fromID, CredentialRef: h.attest(fromID)}
	to := registry.Descriptor{A2AURL: toGatewayURL + "/v1/a2a/" + toID}
	return h.w.SendTask(ctx, from, to, m, true)
}

// gateway is the minimal stand-in: no TLS, the paired host's fingerprint is
// configured rather than read from a connection.
type gateway struct {
	h      *host
	peerFP string

	mu        sync.Mutex
	addr      string
	srv       *http.Server
	dropAfter chan struct{}
	dropped   atomic.Int32
	written   atomic.Int32
}

func (g *gateway) url() string { return "http://" + g.addr }

func (g *gateway) start() {
	g.mu.Lock()
	defer g.mu.Unlock()
	addr := g.addr
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		g.h.t.Fatal(err)
	}
	g.addr = ln.Addr().String()
	g.srv = &http.Server{Handler: g}
	go g.srv.Serve(ln)
}

func (g *gateway) kill() {
	g.mu.Lock()
	srv := g.srv
	g.srv = nil
	g.mu.Unlock()
	if srv != nil {
		srv.Close()
	}
}

// armDrop makes the gateway forward the next request, then, once injected
// closes, die without writing the response.
func (g *gateway) armDrop(injected chan struct{}) {
	g.mu.Lock()
	g.dropAfter = injected
	g.mu.Unlock()
}

func (g *gateway) lookup(id string) (registry.Descriptor, bool) {
	ds, err := registry.New(g.h.state).List()
	if err != nil {
		return registry.Descriptor{}, false
	}
	for _, d := range ds {
		if d.AgentID == id {
			return d, true
		}
	}
	return registry.Descriptor{}, false
}

func (g *gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id, ok := strings.CutPrefix(r.URL.Path, "/v1/a2a/")
	if !ok {
		http.Error(w, "not_found", http.StatusNotFound)
		return
	}
	tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	senderID, ok := strings.CutPrefix(tok, attestPrefix)
	if !ok || senderID == "" {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	d, ok := g.lookup(id)
	if !ok {
		http.Error(w, "no_such_agent", http.StatusNotFound)
		return
	}
	cred, err := os.ReadFile(g.h.credPath)
	if err != nil {
		http.Error(w, "cred", http.StatusInternalServerError)
		return
	}
	body, _ := io.ReadAll(r.Body)
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, d.A2AURL+"/", bytes.NewReader(body))
	for k, v := range r.Header {
		if !strings.EqualFold(k, "Authorization") && !strings.EqualFold(k, a2a.SenderHeader) {
			req.Header[k] = v
		}
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(cred)))
	req.Header.Set(a2a.SenderHeader, "remote:"+g.peerFP+"/"+senderID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, "bad_gateway", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)

	g.mu.Lock()
	drop := g.dropAfter
	g.dropAfter = nil
	g.mu.Unlock()
	if drop != nil {
		select {
		case <-drop:
		case <-time.After(5 * time.Second):
		}
		g.dropped.Add(1)
		g.kill()
		return
	}
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(out)
	g.written.Add(1)
}

// node is one agent session: a shim serving one Codex thread over A2A.
type node struct {
	t      *testing.T
	h      *host
	name   string
	thread string
	fc     *fakeCodex
	sh     *shim.Shim
	srv    *mcpserver.Server
	cs     *mcp.ClientSession

	mu      sync.Mutex
	servers map[string]*a2a.Server
	stopped bool
}

func (n *node) id() string { return identity.AgentID("codex", n.thread) }

func newNode(h *host, ppid int, name, thread string) *node {
	t := h.t
	n := &node{t: t, h: h, name: name, thread: thread, servers: map[string]*a2a.Server{}}
	n.fc = &fakeCodex{Fake: adapter.NewFake(adapter.Codex)}
	n.fc.threads = []codex.LoadedThread{{Endpoint: "fake://" + thread, ID: thread, Name: name, Cwd: "/work/" + name, Status: "idle",
		Session: adapter.SessionStatus{Loaded: true, Reachable: true}}}
	hs := []harness.Harness{codex.NewHarness(codex.HarnessOptions{Adapter: n.fc, Directory: registry.New(h.state)})}
	sh, err := shim.New(shim.Options{StateDir: h.state, PPID: ppid, Harnesses: hs, StartA2A: n.startA2A, OnReply: n.onReply})
	if err != nil {
		t.Fatal(err)
	}
	n.sh = sh
	n.srv = mcpserver.New(sh.MCPOptions())
	st, ct := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := n.srv.MCP().Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	n.cs, err = mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.stop)
	n.ok("receive_message", map[string]any{})
	return n
}

func (n *node) stop() {
	if n.stopped {
		return
	}
	n.stopped = true
	n.cs.Close()
	n.srv.Close()
	n.sh.Close()
}

func (n *node) call(tool string, args any) (bool, map[string]any) {
	n.t.Helper()
	res, err := n.cs.CallTool(context.Background(), &mcp.CallToolParams{Meta: map[string]any{"threadId": n.thread}, Name: tool, Arguments: args})
	if err != nil {
		n.t.Fatalf("%s: protocol error: %v", tool, err)
	}
	out := map[string]any{}
	if res.StructuredContent != nil {
		b, _ := json.Marshal(res.StructuredContent)
		json.Unmarshal(b, &out)
	}
	return res.IsError, out
}

func (n *node) ok(tool string, args any) map[string]any {
	n.t.Helper()
	isErr, out := n.call(tool, args)
	if isErr {
		n.t.Fatalf("%s %s: error result %v", n.name, tool, out)
	}
	return out
}

type agentReceiver struct{ st *shim.AgentState }

func (r agentReceiver) Receive(ctx context.Context, in receiver.Inbound) receiver.Result {
	return r.st.Receive(ctx, in)
}
func (r agentReceiver) Resume(ctx context.Context, in receiver.Inbound) receiver.Result {
	return r.st.Receiver().Resume(ctx, in)
}
func (r agentReceiver) Cancel(id string) (bool, error) { return r.st.Receiver().Cancel(id) }

// startA2A is wire.startA2A with one stub: a header value shaped
// remote:<fp>/<agent_id> resolves to a peer whose "own token" is the host's
// gateway credential. Production needs this as the gateway-credential case in
// a2a.authenticate (R4).
func (n *node) startA2A(_ context.Context, st *shim.AgentState) (shim.A2AListener, error) {
	a := st.Agent()
	id := a.ID()
	tok := filepath.Join(n.h.state, "tokens", id+".token")
	if _, err := udsingress.EnsureKey(tok); err != nil {
		return shim.A2AListener{}, err
	}
	tasks, err := a2a.NewJournalTaskStore(a.Journal(), journal.Options{})
	if err != nil {
		return shim.A2AListener{}, err
	}
	remote := func(key string) receiver.Sender {
		return receiver.Sender{Verified: true, AgentID: key, Name: key, Role: registry.RolePrimary, Harness: "remote"}
	}
	srv := a2a.NewServer(a2a.Config{
		Tasks:    tasks,
		Receiver: agentReceiver{st},
		Profile: func() a2a.Profile {
			return a2a.Profile{Name: a.Name(), Harness: string(a.Harness()), Role: registry.RolePrimary}
		},
		Version:       mcpserver.Version,
		PeerTokenPath: filepath.Join(n.h.state, "peer.token"),
		ResolvePeer: func(name string) (a2a.Peer, bool) {
			if !strings.HasPrefix(name, "remote:") {
				return a2a.Peer{}, false
			}
			return a2a.Peer{AgentID: name, Role: registry.RolePrimary, Harness: "remote", TokenPath: n.h.credPath}, true
		},
		ResolveSender: func(p string) (receiver.Sender, bool) {
			if strings.HasPrefix(p, "remote:") {
				return remote(p), true
			}
			return receiver.Sender{}, false
		},
		OnAskClosed: func(ids []string) {
			for _, m := range ids {
				st.Receiver().CloseRoute(m)
			}
		},
	})
	srv.Resume()
	url, err := srv.Listen()
	if err != nil {
		srv.Close()
		return shim.A2AListener{}, err
	}
	done := make(chan struct{})
	go func() { defer close(done); srv.Serve(context.Background()) }()
	n.mu.Lock()
	n.servers[id] = srv
	n.mu.Unlock()
	return shim.A2AListener{URL: url, CredentialRef: tok, Close: func(ctx context.Context) error {
		err := srv.CloseContext(ctx)
		select {
		case <-done:
		case <-ctx.Done():
		}
		return err
	}}, nil
}

// onReply is wire.onReply plus the remote branch R8 adds to completeReply and
// forwardReply: it completes the ask task, then forwards the reply to the
// asker by agent_id through the asker's gateway. A non-accepted forward is an
// error, which leaves the obligation pending (send.go:562-566).
func (n *node) onReply(ctx context.Context, st *shim.AgentState, original, reply msg.Message) error {
	n.mu.Lock()
	srv := n.servers[st.Agent().ID()]
	n.mu.Unlock()
	if srv == nil {
		return errors.New("no A2A server")
	}
	var text strings.Builder
	for _, p := range reply.Parts {
		text.WriteString(p.Text)
	}
	if err := srv.CompleteAsk(original.MessageID, text.String()); err != nil && !errors.Is(err, a2a.ErrAskClosed) && !errors.Is(err, a2a.ErrNoSuchAsk) {
		return err
	}
	_, askerID, ok := strings.Cut(strings.TrimPrefix(original.From, "remote:"), "/")
	if !ok {
		return nil
	}
	_, state, err := n.h.send(ctx, n.name, st.Agent().ID(), n.h.peerGW(), askerID, reply)
	if state == adapter.Accepted || state == adapter.Queued {
		return nil
	}
	return fmt.Errorf("remote_reply_%s: %v", state, err)
}

func records(t *testing.T, stateDir, agentID string) []journal.Record {
	t.Helper()
	b, err := os.ReadFile(journal.Path(stateDir, agentID))
	if err != nil {
		t.Fatal(err)
	}
	var out []journal.Record
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var r journal.Record
		if json.Unmarshal([]byte(l), &r) == nil {
			out = append(out, r)
		}
	}
	return out
}

func countRecords(rs []journal.Record, typ journal.Type, messageID string) int {
	n := 0
	for _, r := range rs {
		if r.Type == typ && r.MessageID == messageID {
			n++
		}
	}
	return n
}

func lastObligation(rs []journal.Record, replyID string) string {
	st := ""
	for _, r := range rs {
		if r.Type == journal.TypeObligation && r.MessageID == replyID {
			st = r.State
		}
	}
	return st
}

type world struct {
	t          *testing.T
	a, b       *host
	alice, bob *node
}

func newWorld(t *testing.T) *world {
	w := &world{t: t, a: newHost(t, "fpA"), b: newHost(t, "fpB")}
	w.a.gw = &gateway{h: w.a, peerFP: "fpB"}
	w.b.gw = &gateway{h: w.b, peerFP: "fpA"}
	w.a.peerGW = w.b.gw.url
	w.b.peerGW = w.a.gw.url
	w.a.gw.start()
	w.b.gw.start()
	t.Cleanup(func() { w.a.gw.kill(); w.b.gw.kill() })
	w.alice = newNode(w.a, 11, "alice", "T-alice")
	w.bob = newNode(w.b, 12, "bob", "T-bob")
	return w
}

// ask builds alice's ask and journals the outbound record with the host-scoped
// recipient key first, as plan section 8 step 1 says the sender does.
func (w *world) ask(text string) msg.Message {
	id := msg.NewUUIDv7()
	m := msg.Message{MessageID: id, ContextID: id, From: "alice", To: "bob", Kind: msg.Ask, Delivery: msg.Immediate, SentAt: time.Now().UTC(), Parts: []msg.Part{{Type: "text", Text: text}}, HopChain: []string{msg.Hop(w.alice.id())}}
	body, _ := json.Marshal(m)
	err := w.alice.sh.Agent(w.alice.id()).Agent().Journal().Append(journal.Record{Type: journal.TypeOutbound, MessageID: id, To: "remote:fpB/" + w.bob.id(), Kind: string(msg.Ask), State: journal.StateSubmitted, ContextID: id, Message: body})
	if err != nil {
		w.t.Fatal(err)
	}
	return m
}

func (w *world) sendAsk(m msg.Message) (string, adapter.State, error) {
	return w.a.send(context.Background(), "alice", w.alice.id(), w.b.gw.url(), w.bob.id(), m)
}

// dropFirstAsk sends m with B's gateway set to die after bob's harness write
// and before relaying the response; it returns what the asker saw.
func (w *world) dropFirstAsk(m msg.Message) (string, adapter.State, error) {
	injected := make(chan struct{})
	w.bob.fc.ScriptInject(adapter.Outcome{State: adapter.Accepted, After: func(adapter.Call) { close(injected) }})
	w.b.gw.armDrop(injected)
	return w.sendAsk(m)
}

func (w *world) restartB() { w.b.gw.start() }

func TestKillAfterInjectionBeforeResponse(t *testing.T) {
	w := newWorld(t)
	m := w.ask("what is 6*7?")
	w.dropFirstAsk(m)
	if n := len(w.bob.fc.Writes()); n != 1 {
		t.Fatalf("bob's harness has %d writes, want 1", n)
	}
	if w.b.gw.dropped.Load() != 1 || w.b.gw.written.Load() != 0 {
		t.Fatalf("gateway dropped=%d responses written=%d, want 1 and 0", w.b.gw.dropped.Load(), w.b.gw.written.Load())
	}
	if _, err := net.DialTimeout("tcp", w.b.gw.addr, time.Second); err == nil {
		t.Fatal("gateway B still accepts connections")
	}
}

func TestAskerShowsUnknown(t *testing.T) {
	w := newWorld(t)
	_, state, err := w.dropFirstAsk(w.ask("what is 6*7?"))
	t.Logf("asker saw state=%s err=%v", state, err)
	if state != adapter.Unknown {
		t.Fatalf("asker state %q, want unknown", state)
	}
	if n := len(w.bob.fc.Writes()); n != 1 {
		t.Fatalf("bob's harness has %d writes, so unknown was not the truthful answer", n)
	}
}

func TestRetryInjectsOnce(t *testing.T) {
	w := newWorld(t)
	m := w.ask("what is 6*7?")
	w.dropFirstAsk(m)
	w.restartB()
	_, state, err := w.sendAsk(m)
	t.Logf("retry state=%s err=%v", state, err)
	if state != adapter.Accepted {
		t.Fatalf("retry state %q (%v), want accepted", state, err)
	}
	if n := len(w.bob.fc.Writes()); n != 1 {
		t.Fatalf("bob's harness has %d writes after the retry, want 1", n)
	}
	if n := w.bob.fc.Writes()[0].Message.MessageID; n != m.MessageID {
		t.Fatalf("injected message_id %s, want %s", n, m.MessageID)
	}
}

func TestRetryDedupedByReceiver(t *testing.T) {
	w := newWorld(t)
	m := w.ask("what is 6*7?")
	st := w.bob.sh.Agent(w.bob.id())
	in := receiver.Inbound{Ingress: receiver.A2A, Message: m, Sender: receiver.Sender{Verified: true, AgentID: "remote:fpA/" + w.alice.id(), Name: "remote:fpA/" + w.alice.id(), Role: registry.RolePrimary, Harness: "remote"}}
	if r := st.Receive(context.Background(), in); r.Duplicate || r.State != adapter.Accepted {
		t.Fatalf("first receive %+v", r)
	}
	before := countRecords(records(t, w.b.state, w.bob.id()), journal.TypeDedupe, m.MessageID)
	if r := st.Receive(context.Background(), in); !r.Duplicate {
		t.Fatalf("second receive %+v, want Duplicate", r)
	}
	if n := len(w.bob.fc.Writes()); n != 1 {
		t.Fatalf("bob's harness has %d writes, want 1", n)
	}
	if n := countRecords(records(t, w.b.state, w.bob.id()), journal.TypeDedupe, m.MessageID); n != before || n == 0 {
		t.Fatalf("%d dedupe records after the second receive, %d before", n, before)
	}
}

// askAndRetry runs the S4 prefix: dropped ask, gateway restart, retry.
func (w *world) askAndRetry() msg.Message {
	m := w.ask("what is 6*7?")
	w.dropFirstAsk(m)
	w.restartB()
	if _, state, err := w.sendAsk(m); state != adapter.Accepted {
		w.t.Fatalf("retry state %q (%v)", state, err)
	}
	return m
}

func (w *world) reply(ask msg.Message, text string) (bool, map[string]any) {
	return w.bob.call("reply", map[string]any{"message_id": ask.MessageID, "text": text})
}

func TestReplyToGoneAskerNotRedirected(t *testing.T) {
	w := newWorld(t)
	m := w.askAndRetry()
	w.alice.stop()
	reuser := newNode(w.a, 13, "alice", "T-alice-2")
	if got := reuser.sh.Agent(reuser.id()).Agent().Name(); got != "alice" {
		t.Fatalf("new session registered as %q, not alice: the name was not reused", got)
	}
	isErr, out := w.reply(m, "42")
	t.Logf("reply to a gone asker: isErr=%v %v", isErr, out)
	if out["state"] == "accepted" {
		t.Fatalf("reply reported accepted with the asker gone: %v", out)
	}
	if n := len(reuser.fc.Writes()); n != 0 {
		t.Fatalf("name reuser got %d writes", n)
	}
	rs := records(t, w.b.state, w.bob.id())
	for _, r := range rs {
		if r.Type == journal.TypeObligation && r.InReplyTo == m.MessageID && r.State == journal.StateDelivered {
			t.Fatalf("obligation journaled delivered: %+v", r)
		}
	}
}
