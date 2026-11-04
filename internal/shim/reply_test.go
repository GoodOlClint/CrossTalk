package shim

import (
	"context"
	"testing"

	"github.com/GoodOlClint/crosstalk/internal/identity"
	"github.com/GoodOlClint/crosstalk/internal/journal"
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
