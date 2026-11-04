package shim

import "testing"

func TestForwardRingIsStoppedByHopChain(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	w.codex.addThread("T-bob", "bob")
	w.codex.addThread("T-carol", "carol")
	na := w.node(alice.pid, w.lb, nil)
	nbc := w.node(1, w.lb, nil)
	na.ok(nil, "receive_message", map[string]any{})
	nbc.ok(thread("T-bob"), "receive_message", map[string]any{})
	nbc.ok(thread("T-carol"), "receive_message", map[string]any{})

	if r := na.ok(nil, "send_message", send("bob", "start")); r["state"] == "rejected" {
		t.Fatalf("alice to bob %v", r)
	}
	if r := nbc.ok(thread("T-bob"), "send_message", send("carol", "forward")); r["state"] == "rejected" {
		t.Fatalf("bob to carol %v", r)
	}
	if r := nbc.ok(thread("T-carol"), "send_message", send("alice", "forward")); r["state"] != "rejected" || r["reason"] != "hop_repeat" {
		t.Fatalf("carol to alice closed the ring: %v", r)
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
