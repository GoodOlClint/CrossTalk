package shim

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/udsingress"
)

const nudgeMarker = "messages since this server started"

func nudged(r map[string]any) bool {
	h, _ := r["hint"].(string)
	return strings.Contains(h, nudgeMarker)
}

func delivered(r map[string]any) bool { return r["state"] == "accepted" || r["state"] == "queued" }

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

	for _, s := range []struct {
		n    *node
		meta map[string]any
		to   string
	}{{nc, nil, "w1"}, {nc, nil, "w2"}, {nw, thread("T-w1"), "w2"}, {nw, thread("T-w2"), "coord"}} {
		if r := s.n.ok(s.meta, "send_message", send(s.to, "x")); !delivered(r) {
			t.Fatalf("send to %s: %v", s.to, r)
		}
	}
}

func TestNudgeFiresAtEachThresholdAndOnlyThere(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	w.codex.addThread("T-bob", "bob")
	w.codex.addThread("T-carol", "carol")
	na := w.node(alice.pid, w.lb, nil)
	nbc := w.node(1, w.lb, nil)
	na.ok(nil, "receive_message", map[string]any{})
	nbc.ok(thread("T-bob"), "receive_message", map[string]any{})
	nbc.ok(thread("T-carol"), "receive_message", map[string]any{})

	var fired []int
	for i := 1; i <= 245; i++ {
		r := na.ok(nil, "send_message", send("bob", "x"))
		if !delivered(r) {
			t.Fatalf("send %d: %v", i, r)
		}
		if nudged(r) {
			fired = append(fired, i)
		}
		if i == 29 {
			if r := na.ok(nil, "send_message", send("carol", "x")); nudged(r) {
				t.Fatal("bob's count nudged a send to carol")
			}
		}
	}
	if want := []int{30, 60, 120, 240}; len(fired) != len(want) || fired[0] != 30 || fired[1] != 60 || fired[2] != 120 || fired[3] != 240 {
		t.Fatalf("nudged at %v, want %v", fired, want)
	}
	for i := 2; i <= 30; i++ {
		r := na.ok(nil, "send_message", send("carol", "x"))
		if nudged(r) != (i == 30) {
			t.Fatalf("carol send %d nudged=%v", i, nudged(r))
		}
	}

	rs, err := na.sh.Agent(alice.agentID()).agent.Journal().Records()
	if err != nil {
		t.Fatal(err)
	}
	var counts []int
	for _, r := range rs {
		if r.Type == journal.TypeNudge {
			counts = append(counts, r.Count)
			if r.To == "" || r.At.IsZero() {
				t.Fatalf("nudge record %+v lacks peer or time", r)
			}
		}
	}
	if len(counts) != 5 || counts[0] != 30 || counts[3] != 240 || counts[4] != 30 {
		t.Fatalf("journaled nudges %v, want 30 60 120 240 then carol's 30", counts)
	}
	na.stop()
	n2 := w.node(alice.pid, w.lb, nil)
	n2.ok(nil, "receive_message", map[string]any{})
	if r := n2.ok(nil, "send_message", send("bob", "x")); !delivered(r) || nudged(r) {
		t.Fatalf("after restart the count must start over: %v", r)
	}
}

func TestNudgeAppendsToTheNativeOneWayHint(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	w.claude(4002, "sess-bob", "bob")
	na := w.node(alice.pid, nil, nil)
	var got map[string]any
	for i := 1; i <= 30; i++ {
		got = na.ok(nil, "send_message", send("bob", "x"))
	}
	h, _ := got["hint"].(string)
	if !strings.Contains(h, "one way") || !strings.Contains(h, nudgeMarker) {
		t.Fatalf("hint %q", h)
	}
}

func TestNudgeOnAskAndReply(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	w.codex.addThread("T-bob", "bob")
	na := w.node(alice.pid, w.lb, nil)
	nb := w.node(1, w.lb, nil)
	na.ok(nil, "receive_message", map[string]any{})
	nb.ok(thread("T-bob"), "receive_message", map[string]any{})

	var ask map[string]any
	for i := 1; i <= 30; i++ {
		ask = na.ok(nil, "ask", send("bob", "q?"))
	}
	if !nudged(ask) || !delivered(ask) {
		t.Fatalf("30th ask %v", ask)
	}

	id := na.ok(nil, "send_message", send("bob", "start"))["message_id"]
	bobTurn, fired := true, false
	for i := 0; i < 70 && !fired; i++ {
		n, meta := nb, thread("T-bob")
		if !bobTurn {
			n, meta = na, nil
		}
		isErr, out := n.call(meta, "reply", map[string]any{"message_id": id, "text": "again"})
		if isErr || out["message_id"] == nil {
			t.Fatalf("reply %d: %v", i, out)
		}
		fired = nudged(out)
		id = out["message_id"]
		bobTurn = !bobTurn
	}
	if !fired {
		t.Fatal("no reply result carried the nudge")
	}
}

func TestPooledThreadsNudgeSeparately(t *testing.T) {
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
	fired := make([][]int, 2)
	for i, th := range []string{"T-bob", "T-carol"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 1; n <= 61; n++ {
				res, err := nbc.cs.CallTool(context.Background(), &mcp.CallToolParams{Meta: thread(th), Name: "send_message", Arguments: send("alice", "x")})
				if err != nil {
					return
				}
				b, _ := json.Marshal(res.StructuredContent)
				if strings.Contains(string(b), nudgeMarker) {
					fired[i] = append(fired[i], n)
				}
			}
		}()
	}
	wg.Wait()
	for i, f := range fired {
		if len(f) != 2 || f[0] != 30 || f[1] != 60 {
			t.Fatalf("thread %d nudged at %v, want 30 and 60", i, f)
		}
	}
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
