package shim

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/GoodOlClint/crosstalk/internal/harness/codex"
	"github.com/GoodOlClint/crosstalk/internal/identity"
	"github.com/GoodOlClint/crosstalk/internal/mcpserver"
)

// Right after a refresh, before any tool call, informs reach a bound Claude
// agent and two bound idle Codex threads served by one shim. The exec leaves
// the old image's descriptors on disk, still Live (same pid and start time);
// one Codex thread's endpoint and the Claude session file are missing at
// resume and come back a moment later.
func TestRefreshInboundBeforeAnyCall(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	w.codex.addThread("T-bob", "bob")
	w.codex.addThread("T-carol", "carol")
	w.codex.addThread("T-dan", "dan")
	n1 := w.node(alice.pid, w.lb, nil)
	n1.ok(nil, "receive_message", map[string]any{})
	n1.ok(thread("T-bob"), "receive_message", map[string]any{})
	n1.ok(thread("T-carol"), "receive_message", map[string]any{})
	ids := []string{alice.agentID(), identity.AgentID("codex", "T-bob"), identity.AgentID("codex", "T-carol")}
	old := map[string]string{}
	stale := map[string][]byte{}
	for _, id := range ids {
		d, ok := w.descriptor(id)
		if !ok {
			t.Fatalf("%s not bound before the refresh", id)
		}
		old[id] = d.InstanceID
		b, err := os.ReadFile(n1.sh.reg.DescriptorPath(id))
		if err != nil {
			t.Fatal(err)
		}
		stale[id] = b
	}
	ho := handoffOf(t, n1)
	for id, b := range stale {
		if err := os.WriteFile(n1.sh.reg.DescriptorPath(id), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	w.codex.SetRegisterError("T-carol", codex.ErrNoEndpoint)
	session := filepath.Join(w.sessions, itoa(alice.pid)+".json")
	if err := os.Rename(session, session+".away"); err != nil {
		t.Fatal(err)
	}

	sh, err := New(Options{StateDir: w.state, PPID: alice.pid, Harnesses: w.harnesses(w.codex), Transport: w.lb})
	if err != nil {
		t.Fatal(err)
	}
	sh.reg.StartTime = startTime
	w.lb.Attach(sh)
	opt := sh.MCPOptions()
	opt.SupersedeInterval = 50 * time.Millisecond
	srv := mcpserver.New(opt)
	ctx, cancel := context.WithCancel(context.Background())
	srv.Resume(ctx, ho)
	st, ct := mcp.NewInMemoryTransports()
	if _, err := srv.MCP().Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	n2 := &node{t: t, sh: sh, srv: srv, cs: cs}
	t.Cleanup(func() { cancel(); n2.stop() })

	for _, id := range []string{alice.agentID(), identity.AgentID("codex", "T-carol")} {
		if d, ok := w.descriptor(id); ok && d.InstanceID == old[id] {
			t.Fatalf("%s: the old image's descriptor is still advertised while its session does not resolve", id)
		}
	}
	w.codex.SetRegisterError("T-carol", nil)
	if err := os.Rename(session+".away", session); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		waitFor(t, id+" re-registered by the new image", func() bool {
			d, ok := w.descriptor(id)
			return ok && d.InstanceID != old[id]
		})
	}

	sender := w.node(1, w.lb, nil)
	for _, to := range []string{"alice", "bob", "carol"} {
		sender.ok(thread("T-dan"), "send_message", send(to, "hello "+to))
	}
	waitFor(t, "alice to get the inform", func() bool {
		return slices.ContainsFunc(alice.got(), func(s string) bool { return strings.Contains(s, "hello alice") })
	})
	waitFor(t, "both Codex threads to get their informs", func() bool {
		got := map[string]bool{}
		for _, c := range w.codex.Writes() {
			got[c.SessionRef] = true
		}
		return got["T-bob"] && got["T-carol"]
	})
}
