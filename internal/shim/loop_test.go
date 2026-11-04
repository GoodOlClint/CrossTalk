package shim

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/udsingress"
)

type tickClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *tickClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *tickClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newClock() *tickClock { return &tickClock{t: time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)} }

func TestSlowPingPongStopsWithinTheHourlyWindow(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	w.codex.addThread("T-bob", "bob")
	clk := newClock()
	na := w.node(alice.pid, w.lb, func(o *Options) { o.Now = clk.now })
	nb := w.node(1, w.lb, func(o *Options) { o.Now = clk.now })
	na.ok(nil, "receive_message", map[string]any{})
	nb.ok(thread("T-bob"), "receive_message", map[string]any{})

	start := clk.now()
	for round := 0; round < 200; round++ {
		if r := na.ok(nil, "send_message", send("bob", "ping")); r["reason"] == ReasonLoopCap {
			if clk.now().Sub(start) >= time.Hour {
				t.Fatalf("stopped after the hour, round %d", round)
			}
			clk.add(61 * time.Minute)
			if r := na.ok(nil, "send_message", send("bob", "ping")); r["reason"] == ReasonLoopCap {
				t.Fatalf("still capped an hour later: %v", r)
			}
			return
		}
		nb.ok(thread("T-bob"), "send_message", send("alice", "pong"))
		clk.add(31 * time.Second)
	}
	t.Fatal("200 slow rounds without a stop")
}

func TestFailedSendsAreNotCounted(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	w.codex.addThread("T-bob", "bob")
	na := w.node(alice.pid, w.lb, nil)
	nb := w.node(1, w.lb, nil)
	na.ok(nil, "receive_message", map[string]any{})
	nb.ok(thread("T-bob"), "receive_message", map[string]any{})

	down := adapter.Outcome{State: adapter.Unreachable, Err: errors.New("down")}
	for i := 0; i < 70; i++ {
		w.codex.ScriptInject(down)
		if r := na.ok(nil, "send_message", send("bob", "retry")); r["reason"] == ReasonLoopCap {
			t.Fatalf("failed send %d counted: %v", i, r)
		}
	}
	if r := na.ok(nil, "send_message", send("bob", "now")); r["state"] != "accepted" && r["state"] != "queued" {
		t.Fatalf("send after the peer returned: %v", r)
	}
}

func TestWorkerStatusToCoordinatorAfterWorkersTalked(t *testing.T) {
	w := newWorld(t)
	coord := w.claude(4001, "sess-coord", "coord")
	w.codex.addThread("T-w1", "w1")
	w.codex.addThread("T-w2", "w2")
	nc := w.node(coord.pid, w.lb, nil)
	nw := w.node(1, w.lb, nil)
	nc.ok(nil, "receive_message", map[string]any{})
	nw.ok(thread("T-w1"), "receive_message", map[string]any{})
	nw.ok(thread("T-w2"), "receive_message", map[string]any{})

	nc.ok(nil, "send_message", send("w1", "kickoff"))
	nc.ok(nil, "send_message", send("w2", "kickoff"))
	nw.ok(thread("T-w1"), "send_message", send("w2", "which api?"))
	if r := nw.ok(thread("T-w2"), "send_message", send("coord", "status: done")); r["state"] == "rejected" {
		t.Fatalf("status report %v", r)
	}
}

const (
	pinShortCap    = 20
	pinShortWindow = 10 * time.Minute
)

func TestShortCapNumberAndWindowArePinned(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	w.codex.addThread("T-bob", "bob")
	w.codex.addThread("T-carol", "carol")
	clk := newClock()
	na := w.node(alice.pid, w.lb, func(o *Options) { o.Now = clk.now })
	nbc := w.node(1, w.lb, nil)
	na.ok(nil, "receive_message", map[string]any{})
	nbc.ok(thread("T-bob"), "receive_message", map[string]any{})
	nbc.ok(thread("T-carol"), "receive_message", map[string]any{})

	for i := 0; i < pinShortCap; i++ {
		if r := na.ok(nil, "send_message", send("bob", "x")); r["reason"] == ReasonLoopCap {
			t.Fatalf("send %d of %d capped", i+1, pinShortCap)
		}
	}
	if r := na.ok(nil, "send_message", send("bob", "x")); r["reason"] != ReasonLoopCap {
		t.Fatalf("send %d not capped: %v", pinShortCap+1, r)
	}
	if r := na.ok(nil, "send_message", send("carol", "x")); r["reason"] == ReasonLoopCap {
		t.Fatalf("bob's count capped carol: %v", r)
	}
	clk.add(pinShortWindow - time.Second)
	if r := na.ok(nil, "send_message", send("bob", "x")); r["reason"] != ReasonLoopCap {
		t.Fatalf("window ended early: %v", r)
	}
	clk.add(2 * time.Second)
	if r := na.ok(nil, "send_message", send("bob", "x")); r["reason"] == ReasonLoopCap {
		t.Fatalf("window did not end: %v", r)
	}
}

func TestUnknownSendsAreCounted(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	w.codex.addThread("T-bob", "bob")
	na := w.node(alice.pid, w.lb, nil)
	nb := w.node(1, w.lb, nil)
	na.ok(nil, "receive_message", map[string]any{})
	nb.ok(thread("T-bob"), "receive_message", map[string]any{})

	slow := adapter.Outcome{State: adapter.Unknown}
	for i := 0; i < pinShortCap+5; i++ {
		w.codex.ScriptInject(slow)
		if r := na.ok(nil, "send_message", send("bob", "x")); r["reason"] == ReasonLoopCap {
			return
		}
	}
	t.Fatal("unknown sends never counted")
}

func TestRingStopsOnTheCaps(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	w.codex.addThread("T-bob", "bob")
	w.codex.addThread("T-carol", "carol")
	na := w.node(alice.pid, w.lb, nil)
	nbc := w.node(1, w.lb, nil)
	na.ok(nil, "receive_message", map[string]any{})
	nbc.ok(thread("T-bob"), "receive_message", map[string]any{})
	nbc.ok(thread("T-carol"), "receive_message", map[string]any{})

	for i := 0; i < 100; i++ {
		for _, step := range []struct {
			n    *node
			meta map[string]any
			to   string
		}{{na, nil, "bob"}, {nbc, thread("T-bob"), "carol"}, {nbc, thread("T-carol"), "alice"}} {
			r := step.n.ok(step.meta, "send_message", send(step.to, "around"))
			if r["reason"] == ReasonLoopCap {
				return
			}
			if r["state"] == "rejected" {
				t.Fatalf("ring step %d to %s: %v", i, step.to, r)
			}
		}
	}
	t.Fatal("the ring ran 100 times without a stop")
}

func TestPooledThreadsCountSeparately(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	w.codex.addThread("T-bob", "bob")
	w.codex.addThread("T-carol", "carol")
	na := w.node(alice.pid, w.lb, nil)
	nbc := w.node(1, w.lb, nil)
	na.ok(nil, "receive_message", map[string]any{})
	nbc.ok(thread("T-bob"), "receive_message", map[string]any{})
	nbc.ok(thread("T-carol"), "receive_message", map[string]any{})

	var wg sync.WaitGroup
	capped := make([]int, 2)
	for i, th := range []string{"T-bob", "T-carol"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < SendCapShort+5; n++ {
				res, err := nbc.cs.CallTool(context.Background(), &mcp.CallToolParams{Meta: thread(th), Name: "send_message", Arguments: send("alice", "x")})
				if err != nil {
					return
				}
				b, _ := json.Marshal(res.StructuredContent)
				if strings.Contains(string(b), ReasonLoopCap) {
					capped[i]++
				}
			}
		}()
	}
	wg.Wait()
	if capped[0] != 5 || capped[1] != 5 {
		t.Fatalf("capped sends per thread %v, want 5 each", capped)
	}
}

func TestPingPongStopsAtLoopCap(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	w.codex.addThread("T-bob", "bob")
	na := w.node(alice.pid, w.lb, nil)
	nb := w.node(1, w.lb, nil)
	na.ok(nil, "receive_message", map[string]any{})
	nb.ok(thread("T-bob"), "receive_message", map[string]any{})

	for i := 0; i < 100; i++ {
		if r := na.ok(nil, "send_message", send("bob", "ping")); r["reason"] == ReasonLoopCap {
			return
		}
		if r := nb.ok(thread("T-bob"), "send_message", send("alice", "pong")); r["reason"] == ReasonLoopCap {
			return
		}
	}
	t.Fatal("two sessions messaged each other 100 times without a stop")
}

func TestNativeSendsStopAtLoopCap(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	w.claude(4002, "sess-bob", "bob")
	na := w.node(alice.pid, nil, nil)

	for i := 0; i < 100; i++ {
		if r := na.ok(nil, "send_message", send("bob", "ping")); r["reason"] == ReasonLoopCap {
			if h, _ := r["hint"].(string); h == "" {
				t.Fatalf("no hint on %v", r)
			}
			return
		}
	}
	t.Fatal("100 sends to one native session without a stop")
}

func TestReplyChainStopsAtLoopCap(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	w.codex.addThread("T-bob", "bob")
	na := w.node(alice.pid, w.lb, nil)
	nb := w.node(1, w.lb, nil)
	na.ok(nil, "receive_message", map[string]any{})
	nb.ok(thread("T-bob"), "receive_message", map[string]any{})

	id := na.ok(nil, "send_message", send("bob", "start"))["message_id"]
	bobTurn := true
	for i := 0; i < 100; i++ {
		var n *node
		var meta map[string]any
		if bobTurn {
			n, meta = nb, thread("T-bob")
		} else {
			n, meta = na, nil
		}
		isErr, out := n.call(meta, "reply", map[string]any{"message_id": id, "text": "again"})
		if out["reason"] == ReasonLoopCap {
			return
		}
		if isErr || out["state"] == "rejected" || out["message_id"] == nil {
			t.Fatalf("reply %d: %v", i, out)
		}
		id = out["message_id"]
		bobTurn = !bobTurn
	}
	t.Fatal("100 replies without a stop")
}

func TestScriptSendEntersTheRealReceiverChainAsUnverified(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	w.codex.addThread("T-bob", "bob")
	na := w.node(alice.pid, w.lb, nil)
	nb := w.node(1, w.lb, nil)
	na.ok(nil, "receive_message", map[string]any{})
	nb.ok(thread("T-bob"), "receive_message", map[string]any{})
	d, ok := w.descriptor(alice.agentID())
	if !ok {
		t.Fatal("alice not registered")
	}
	ack, err := udsingress.Send(d.UDSPath, "backup finished", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if ack.State != "accepted" {
		t.Fatalf("ack %+v", ack)
	}
	got := alice.got()
	if len(got) != 1 || !strings.Contains(got[0], "backup finished") || !strings.Contains(got[0], "unverified") {
		t.Fatalf("alice got %q", got)
	}
	if rs, _ := na.sh.Agent(alice.agentID()).agent.Journal().Records(); len(rs) == 0 {
		t.Fatal("receiver chain journaled nothing")
	}
}
