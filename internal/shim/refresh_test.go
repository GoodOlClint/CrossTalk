package shim

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/GoodOlClint/crosstalk/internal/identity"
	"github.com/GoodOlClint/crosstalk/internal/mcpserver"
	"github.com/GoodOlClint/crosstalk/internal/msg"
)

// resumed is w.node for a process that starts from a refresh handoff: the
// carried agents are registered before the client connects.
func (w *world) resumed(ppid int, tr Transport, edit func(*Options), ho *mcpserver.Handoff) *node {
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
	srv.Resume(context.Background(), ho)
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

// handoffOf takes n's handoff through the file the real one uses and stops n,
// as the exec ends the old image.
func handoffOf(t *testing.T, n *node) *mcpserver.Handoff {
	t.Helper()
	f, err := mcpserver.WriteHandoff(n.srv.Handoff(mcpserver.SessionState{}, mcpserver.BinaryIdentity{}))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	ho, err := mcpserver.ReadHandoff(f)
	if err != nil {
		t.Fatal(err)
	}
	n.stop()
	return ho
}

func TestResumeReRegistersBoundAgents(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	w.codex.addThread("T-bob", "bob")
	n1 := w.node(alice.pid, nil, nil)
	n1.ok(nil, "receive_message", map[string]any{})
	n1.ok(thread("T-bob"), "receive_message", map[string]any{})
	bob := identity.AgentID("codex", "T-bob")
	old := map[string]string{}
	for _, id := range []string{alice.agentID(), bob} {
		d, ok := w.descriptor(id)
		if !ok {
			t.Fatalf("%s not registered", id)
		}
		old[id] = d.InstanceID
	}
	ho := handoffOf(t, n1)
	for id := range old {
		if _, ok := w.descriptor(id); ok {
			t.Fatalf("%s still live after the old image stopped", id)
		}
	}

	w.resumed(alice.pid, nil, nil, ho)
	for id, inst := range old {
		d, ok := w.descriptor(id)
		if !ok || d.InstanceID == inst || d.UDSPath == "" {
			t.Fatalf("%s after resume, before any call: live=%v descriptor %+v (old instance %s)", id, ok, d, inst)
		}
	}
}

func TestResumeKeepsBoundAtHint(t *testing.T) {
	w := newWorld(t)
	w.codex.addThread("T-bob", "bob")
	now := time.Now()
	n1 := w.node(1, nil, func(o *Options) { o.Now = fakeClock(now.Add(-time.Hour)) })
	n1.ok(thread("T-bob"), "receive_message", map[string]any{})
	ho := handoffOf(t, n1)

	n2 := w.resumed(1, nil, func(o *Options) { o.Now = fakeClock(now.Add(time.Hour)) }, ho)
	_, out := n2.call(thread("T-bob"), "reply", map[string]any{"message_id": msg.NewUUIDv7(), "text": "x"})
	if out["reason"] != ReasonNoSuchMessage || out["hint"] != hintNoRouteUnbound {
		t.Fatalf("reply to an id minted after the first bind = %v, want the unbound hint (the carried boundAt, not the refresh time)", out)
	}
}

func TestResumeRearmsDeadlines(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	w.codex.addThread("T-bob", "bob")
	t0 := time.Date(2026, 11, 3, 13, 0, 0, 0, time.UTC)
	n1 := w.node(alice.pid, pendingTransport{taskID: "task-far"}, func(o *Options) { o.Now = fakeClock(t0) })
	w.node(1, w.lb, nil).ok(thread("T-bob"), "receive_message", map[string]any{})
	n1.ok(nil, "ask", map[string]any{"to": "bob", "text": "slow?", "deadline_seconds": 60})
	ho := handoffOf(t, n1)
	if len(alice.got()) != 0 {
		t.Fatalf("expired before the refresh: %v", alice.got())
	}

	w.resumed(alice.pid, nil, func(o *Options) { o.Now = fakeClock(t0.Add(60*time.Second - 50*time.Millisecond)) }, ho)
	waitFor(t, "the ask deadline to fire after the refresh", func() bool { return len(alice.got()) == 1 })
	if got := alice.got()[0]; !strings.Contains(got, "expired") {
		t.Fatalf("alice got %q", got)
	}
}
