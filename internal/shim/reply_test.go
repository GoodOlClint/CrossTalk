package shim

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/identity"
	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/mcpserver"
	"github.com/GoodOlClint/crosstalk/internal/msg"
)

func TestIdenticalReplyRetryReturnsFirstResult(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	w.codex.addThread("T-bob", "bob")
	na := w.node(alice.pid, w.lb, nil)
	nb := w.node(1, w.lb, nil)
	nb.ok(thread("T-bob"), "receive_message", map[string]any{})
	ask := na.ok(nil, "ask", send("bob", "question?"))
	args := map[string]any{"message_id": ask["message_id"], "text": "answer"}
	first := nb.ok(thread("T-bob"), "reply", args)
	second := nb.ok(thread("T-bob"), "reply", args)
	if first["message_id"] != second["message_id"] || first["state"] != second["state"] || first["state"] != "accepted" {
		t.Fatalf("retry result %v, first %v", second, first)
	}
	if len(alice.got()) != 1 {
		t.Fatalf("identical reply retry injected %d times", len(alice.got()))
	}
}

func TestPendingReplyRetryResumes(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	w.codex.addThread("T-bob", "bob")
	na := w.node(alice.pid, w.lb, nil)
	hooks := 0
	nb := w.node(1, w.lb, func(o *Options) {
		o.OnReply = func(context.Context, *AgentState, msg.Message, msg.Message) error { hooks++; return nil }
	})
	nb.ok(thread("T-bob"), "receive_message", map[string]any{})
	ask := na.ok(nil, "ask", send("bob", "question?"))
	askID := ask["message_id"].(string)

	st := nb.sh.Agent(identity.AgentID("codex", "T-bob"))
	orig, key, _ := st.inbound(askID)
	r := st.newMessage(orig.From, msg.Inform, msg.Immediate, "answer")
	r.InReplyTo, r.ContextID = &askID, orig.ContextID
	if err := st.agent.Journal().Append(st.obligation(r, key, journal.StatePending, true, "")); err != nil {
		t.Fatal(err)
	}

	out := nb.ok(thread("T-bob"), "reply", map[string]any{"message_id": askID, "text": "answer"})
	if out["message_id"] != r.MessageID || out["state"] != "accepted" || hooks != 1 {
		t.Fatalf("resume %v hooks %d, want %s", out, hooks, r.MessageID)
	}
	if len(alice.got()) != 1 {
		t.Fatalf("alice got %q", alice.got())
	}
	if l, _ := st.agent.Journal().Latest(journal.Record{Type: journal.TypeObligation, MessageID: r.MessageID}); l.State != journal.StateDelivered {
		t.Fatalf("obligation %+v", l)
	}
	nb.fails(thread("T-bob"), "reply", map[string]any{"message_id": askID, "text": "other"}, "rejected", ReasonAlreadyReplied)
}

func TestReplyToExpiredInformIsNoSuchMessage(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	w.codex.addThread("T-bob", "bob")
	var mu sync.Mutex
	now := time.Date(2026, 11, 3, 13, 0, 0, 0, time.UTC)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
	na := w.node(alice.pid, w.lb, nil)
	nb := w.node(1, w.lb, func(o *Options) { o.Now = clock })
	nb.ok(thread("T-bob"), "receive_message", map[string]any{})

	old := na.ok(nil, "send_message", send("bob", "fyi"))["message_id"]
	advance(61 * time.Minute)
	fresh := na.ok(nil, "send_message", send("bob", "fyi again"))["message_id"]
	nb.fails(thread("T-bob"), "reply", map[string]any{"message_id": old, "text": "late"}, mcpserver.CodeInvalid, ReasonNoSuchMessage)
	if rep := nb.ok(thread("T-bob"), "reply", map[string]any{"message_id": fresh, "text": "thanks"}); rep["state"] != "accepted" {
		t.Fatalf("reply to fresh inform %v", rep)
	}
	if len(alice.got()) != 1 {
		t.Fatalf("alice got %q", alice.got())
	}
}

func TestAlreadyRepliedSaysNotDeliveredAndNamesSendMessage(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	w.codex.addThread("T-bob", "bob")
	na := w.node(alice.pid, w.lb, nil)
	nb := w.node(1, w.lb, nil)
	nb.ok(thread("T-bob"), "receive_message", map[string]any{})
	askID := na.ok(nil, "ask", send("bob", "question?"))["message_id"]
	nb.ok(thread("T-bob"), "reply", map[string]any{"message_id": askID, "text": "first"})

	isErr, out := nb.call(thread("T-bob"), "reply", map[string]any{"message_id": askID, "text": "second"})
	hint, _ := out["hint"].(string)
	if !isErr || out["reason"] != ReasonAlreadyReplied || !strings.Contains(hint, "NOT delivered") || !strings.Contains(hint, `send_message with to "`+alice.agentID()+`"`) {
		t.Fatalf("already_replied result %v %v", isErr, out)
	}
	if len(alice.got()) != 1 {
		t.Fatalf("alice got %q", alice.got())
	}
}

func TestNotDeliveredHintTarget(t *testing.T) {
	id := identity.AgentID("claude", "sess-alice")
	for _, c := range []struct{ name, key, want string }{
		{"alice", id, `to "` + id + `" and`},
		{"alice-renamed", id, `to "` + id + `" and`},
		{"Alice \"x\"\nnext", "local-user", `to "Alice \"x\"\nnext" and`},
	} {
		got := notDeliveredHint(msg.Message{From: c.name}, c.key)
		if !strings.Contains(got, "NOT delivered") || !strings.Contains(got, c.want) {
			t.Errorf("name %q key %q: %q lacks %q", c.name, c.key, got, c.want)
		}
	}
}

func TestNotDeliveredHintRemoteSenderNamesNoDestination(t *testing.T) {
	key := "remote:abcdef/" + identity.AgentID("claude", "sess-alice")
	got := notDeliveredHint(msg.Message{From: "alice@lab"}, key)
	if !strings.Contains(got, "NOT delivered") || strings.Contains(got, "alice") || !strings.Contains(got, "list_agents") {
		t.Fatalf("remote hint %q", got)
	}
}
