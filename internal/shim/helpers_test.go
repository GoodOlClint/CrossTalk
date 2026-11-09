package shim

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/harness"
	"github.com/GoodOlClint/crosstalk/internal/harness/claude"
	"github.com/GoodOlClint/crosstalk/internal/harness/codex"
	"github.com/GoodOlClint/crosstalk/internal/identity"
	"github.com/GoodOlClint/crosstalk/internal/mcpserver"
	"github.com/GoodOlClint/crosstalk/internal/procstart"
	"github.com/GoodOlClint/crosstalk/internal/registry"
	"github.com/GoodOlClint/crosstalk/internal/udsingress"
)

var fakeStart = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

func fakePID(pid int) bool { return pid >= 4000 && pid < 5000 }

func startTime(pid int) (time.Time, error) {
	if fakePID(pid) && pid != os.Getpid() {
		return fakeStart, nil
	}
	return procstart.StartTime(pid)
}

// shortDir keeps socket paths under the 104-byte macOS limit.
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "sh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

type fakeCodex struct {
	*adapter.Fake
	mu         sync.Mutex
	threads    []codex.LoadedThread
	backend    string
	backendEps []string
}

func (f *fakeCodex) Backend(_ context.Context, endpoint string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.backendEps = append(f.backendEps, endpoint)
	return f.backend
}

func (f *fakeCodex) setBackend(v string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.backend = v
}

func newFakeCodex() *fakeCodex { return &fakeCodex{Fake: adapter.NewFake(adapter.Codex)} }

func (f *fakeCodex) LoadedThreads(context.Context, ...string) []codex.LoadedThread {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]codex.LoadedThread(nil), f.threads...)
}

func (f *fakeCodex) addThread(id, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.threads = append(f.threads, codex.LoadedThread{Endpoint: "fake://" + id, ID: id, Name: name, Cwd: "/work/" + name, Status: "idle",
		Session: adapter.SessionStatus{Loaded: true, Reachable: true}})
}

// fakeClaude is a Claude session: its session file, key and messaging
// socket, which acks every two-line frame.
type fakeClaude struct {
	pid       int
	sessionID string
	name      string
	sock      string
	token     string

	mu       sync.Mutex
	contents []string
	tokens   []string
}

func (w *world) claude(pid int, sessionID, name string) *fakeClaude {
	t := w.t
	f := &fakeClaude{pid: pid, sessionID: sessionID, name: name, sock: filepath.Join(w.sockDir, name+".sock"), token: "tok-" + name}
	rec := map[string]any{"pid": pid, "sessionId": sessionID, "name": name, "cwd": "/work/" + name, "status": "idle", "messagingSocketPath": f.sock}
	b, _ := json.Marshal(rec)
	if err := os.WriteFile(filepath.Join(w.sessions, itoa(pid)+".json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.sessions, itoa(pid)+".abc.key"), []byte(f.token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", f.sock)
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
			go f.handle(c)
		}
	}()
	return f
}

func (f *fakeClaude) handle(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	var auth struct{ Token string }
	var user struct {
		Message struct{ Content string }
	}
	l1, err := r.ReadBytes('\n')
	if err != nil || json.Unmarshal(l1, &auth) != nil {
		return
	}
	l2, err := r.ReadBytes('\n')
	if err != nil || json.Unmarshal(l2, &user) != nil {
		return
	}
	f.mu.Lock()
	f.tokens = append(f.tokens, auth.Token)
	f.contents = append(f.contents, user.Message.Content)
	f.mu.Unlock()
	c.Write([]byte(`{"type":"ack"}` + "\n"))
}

func (f *fakeClaude) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.contents...)
}

func (f *fakeClaude) agentID() string { return identity.AgentID("claude", f.sessionID) }

type world struct {
	t        *testing.T
	state    string
	sessions string
	sockDir  string
	codex    *fakeCodex
	lb       *Loopback
}

func newWorld(t *testing.T) *world {
	w := &world{t: t, state: shortDir(t), sessions: shortDir(t), sockDir: shortDir(t), codex: newFakeCodex(), lb: NewLoopback()}
	return w
}

// become rewrites f's session file as /clear or /resume does: same process and
// socket, another session.
func (w *world) become(f *fakeClaude, sessionID, name string) {
	w.t.Helper()
	f.sessionID, f.name = sessionID, name
	rec := map[string]any{"pid": f.pid, "sessionId": sessionID, "name": name, "cwd": "/work/" + name, "status": "idle", "messagingSocketPath": f.sock}
	b, _ := json.Marshal(rec)
	if err := os.WriteFile(filepath.Join(w.sessions, itoa(f.pid)+".json"), b, 0o600); err != nil {
		w.t.Fatal(err)
	}
}

// harnesses is the production order: Codex over cx, then Claude on the
// world's sessions directory.
func (w *world) harnesses(cx codex.Threads) []harness.Harness {
	ch, err := claude.NewHarness(claude.HarnessOptions{SessionsDir: w.sessions, Alive: fakePID, Ingress: udsingress.NewIngress})
	if err != nil {
		w.t.Fatal(err)
	}
	return []harness.Harness{codex.NewHarness(codex.HarnessOptions{Adapter: cx, Directory: registry.New(w.state)}), ch}
}

type node struct {
	t   *testing.T
	sh  *Shim
	srv *mcpserver.Server
	cs  *mcp.ClientSession
}

func (w *world) node(ppid int, tr Transport, edit func(*Options)) *node {
	t := w.t
	opt := Options{StateDir: w.state, PPID: ppid, Harnesses: w.harnesses(w.codex), Transport: tr}
	if edit != nil {
		edit(&opt)
	}
	sh, err := New(opt)
	if err != nil {
		t.Fatal(err)
	}
	sh.reg.StartTime = startTime
	if lb, ok := tr.(*Loopback); ok {
		lb.Attach(sh)
	}
	srv := mcpserver.New(sh.MCPOptions())
	st, ct := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := srv.MCP().Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	n := &node{t: t, sh: sh, srv: srv, cs: cs}
	t.Cleanup(func() { n.stop() })
	return n
}

func (n *node) stop() {
	n.cs.Close()
	if err := n.srv.Close(); err != nil {
		n.t.Error(err)
	}
	if err := n.sh.Close(); err != nil {
		n.t.Error(err)
	}
}

func thread(id string) map[string]any { return map[string]any{"threadId": id} }

func (n *node) call(meta map[string]any, tool string, args any) (bool, map[string]any) {
	n.t.Helper()
	res, err := n.cs.CallTool(context.Background(), &mcp.CallToolParams{Meta: meta, Name: tool, Arguments: args})
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

func (n *node) ok(meta map[string]any, tool string, args any) map[string]any {
	n.t.Helper()
	isErr, out := n.call(meta, tool, args)
	if isErr {
		n.t.Fatalf("%s %v: error result %v", tool, meta, out)
	}
	return out
}

func (n *node) fails(meta map[string]any, tool string, args any, code, reason string) {
	n.t.Helper()
	isErr, out := n.call(meta, tool, args)
	if !isErr || out["error"] != code || (reason != "" && out["reason"] != reason) {
		n.t.Fatalf("%s %v: got isError=%v %v, want %s/%s", tool, meta, isErr, out, code, reason)
	}
}

func (w *world) descriptor(agentID string) (registry.Descriptor, bool) {
	ds, err := registry.New(w.state).List()
	if err != nil {
		w.t.Fatal(err)
	}
	for _, d := range ds {
		if d.AgentID == agentID {
			return d, true
		}
	}
	return registry.Descriptor{}, false
}

func send(to, text string) map[string]any { return map[string]any{"to": to, "text": text} }

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

func (w *world) claudeAgentID(sessionID string) string { return identity.AgentID("claude", sessionID) }
