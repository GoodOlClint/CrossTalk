package shim

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/GoodOlClint/crosstalk/internal/mcpserver"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

func TestDeadHolderInAnotherProcessYieldsTheName(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	bob := w.claude(4002, "sess-1", "bob")
	carol := w.claude(4003, "sess-c", "carol")
	w.become(carol, "sess-c", "bob")
	na := w.node(alice.pid, w.lb, nil)
	nb := w.node(bob.pid, w.lb, nil)
	nc := w.node(carol.pid, w.lb, nil)
	recv(na, nil)
	recv(nb, nil)

	w.become(bob, "sess-2", "other")
	n, _ := recv(nc, nil)["notice"].(string)
	if !strings.Contains(n, "has ended") || strings.Contains(n, "live") {
		t.Fatalf("notice %q", n)
	}
	if res := na.ok(nil, "ask", send("bob", "q?")); res["reason"] == ReasonNativeInformOnly || res["hint"] != nil {
		t.Fatalf("name of a bound session under its fallback alias: %v", res)
	}

	nb.srv.RetireSuperseded()
	n, _ = recv(nc, nil)["notice"].(string)
	if d, _ := w.descriptor(carol.agentID()); d.Name != "bob" || !strings.Contains(n, "now registered") {
		t.Fatalf("carol %+v notice %q", d, n)
	}
}

func TestRunRetiresSupersededWithoutACall(t *testing.T) {
	w := newWorld(t)
	bob := w.claude(4002, "sess-1", "bob")
	sh, err := New(Options{StateDir: w.state, PPID: bob.pid, Harnesses: w.harnesses(w.codex)})
	if err != nil {
		t.Fatal(err)
	}
	sh.reg.StartTime = startTime
	o := sh.MCPOptions()
	o.SupersedeInterval = 10 * time.Millisecond
	srv := mcpserver.New(o)
	st, ct := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx, st) }()
	t.Cleanup(func() {
		cancel()
		<-done
		sh.Close()
	})
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "receive_message", Arguments: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	oldID := bob.agentID()
	reg := registry.New(w.state)
	if _, err := os.Stat(reg.DescriptorPath(oldID)); err != nil {
		t.Fatal(err)
	}

	w.become(bob, "sess-2", "bob")
	waitFor(t, "the old descriptor to go", func() bool {
		_, err := os.Stat(reg.DescriptorPath(oldID))
		return errors.Is(err, os.ErrNotExist)
	})
	g, err := reg.Acquire(registry.Request{AgentID: "ffffffffffffffff", HarnessName: "bob", Harness: "claude", Cwd: "/w"})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if g.Name() != "bob" {
		t.Fatalf("name lock still held: got %q", g.Name())
	}
}
