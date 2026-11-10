package shim

import (
	"context"
	"sync"
	"testing"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/harness"
	"github.com/GoodOlClint/crosstalk/internal/identity"
	"github.com/GoodOlClint/crosstalk/internal/msg"
)

type signalHarness struct {
	harness.Harness
	mu      sync.Mutex
	signals []string
	onCall  func(msg.Message)
}

func (h *signalHarness) MailSignal(_ context.Context, m msg.Message, _ adapter.Delivery) error {
	if h.onCall != nil {
		h.onCall(m)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.signals = append(h.signals, m.MessageID)
	return nil
}

func messageIDs(out map[string]any) []string {
	var ids []string
	for _, e := range out["messages"].([]any) {
		ids = append(ids, e.(map[string]any)["message_id"].(string))
	}
	return ids
}

func TestReply_ToMailboxedEntryNeverPulled_ExpectNotCollected(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	w.codex.addThread("T-bob", "bob")
	w.codex.SetDefaultInject(adapter.Outcome{State: adapter.Mailboxed})
	na := w.node(alice.pid, w.lb, nil)
	nb := w.node(1, w.lb, nil)
	nb.ok(thread("T-bob"), "receive_message", map[string]any{})
	ask := na.ok(nil, "ask", send("bob", "question?"))
	if ask["state"] != "mailboxed" {
		t.Fatalf("ask %v", ask)
	}
	nb.fails(thread("T-bob"), "reply", map[string]any{"message_id": ask["message_id"], "text": "answer"}, "invalid", ReasonNotCollected)
	if len(alice.got()) != 0 {
		t.Fatalf("a reply to an uncollected entry reached the asker: %q", alice.got())
	}
	out := nb.ok(thread("T-bob"), "receive_message", map[string]any{})
	if ids := messageIDs(out); len(ids) != 1 || ids[0] != ask["message_id"] {
		t.Fatalf("pull %v", out)
	}
}

func TestReply_AfterReReturn_ExpectConfirmedAndNeverReturnedAgain(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	w.codex.addThread("T-bob", "bob")
	w.codex.SetDefaultInject(adapter.Outcome{State: adapter.Mailboxed})
	na := w.node(alice.pid, w.lb, nil)
	nb := w.node(1, w.lb, nil)
	nb.ok(thread("T-bob"), "receive_message", map[string]any{})
	ask := na.ok(nil, "ask", send("bob", "question?"))
	first := nb.ok(thread("T-bob"), "receive_message", map[string]any{})
	if first["messages"].([]any)[0].(map[string]any)["possibly_seen"] != nil {
		t.Fatalf("first pull flagged possibly_seen: %v", first)
	}
	second := nb.ok(thread("T-bob"), "receive_message", map[string]any{})
	if second["messages"].([]any)[0].(map[string]any)["possibly_seen"] != true {
		t.Fatalf("second pull: %v", second)
	}
	nb.ok(thread("T-bob"), "reply", map[string]any{"message_id": ask["message_id"], "text": "answer"})
	if out := nb.ok(thread("T-bob"), "receive_message", map[string]any{}); len(messageIDs(out)) != 0 {
		t.Fatalf("pull after reply %v", out)
	}
}

func TestReply_BeforeSecondPull_ExpectConfirmedEntryNotReReturned(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	w.codex.addThread("T-bob", "bob")
	w.codex.SetDefaultInject(adapter.Outcome{State: adapter.Mailboxed})
	na := w.node(alice.pid, w.lb, nil)
	nb := w.node(1, w.lb, nil)
	nb.ok(thread("T-bob"), "receive_message", map[string]any{})
	ask := na.ok(nil, "ask", send("bob", "question?"))
	nb.ok(thread("T-bob"), "receive_message", map[string]any{})
	nb.ok(thread("T-bob"), "reply", map[string]any{"message_id": ask["message_id"], "text": "answer"})
	if out := nb.ok(thread("T-bob"), "receive_message", map[string]any{}); len(messageIDs(out)) != 0 {
		t.Fatalf("confirmed entry returned again %v", out)
	}
}

func TestPullAndReply_OtherThreadOfSameProcess_ExpectNoDeliveryOrConfirmation(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	w.codex.addThread("T-bob", "bob")
	w.codex.addThread("T-carol", "carol")
	w.codex.SetDefaultInject(adapter.Outcome{State: adapter.Mailboxed})
	na := w.node(alice.pid, w.lb, nil)
	nb := w.node(1, w.lb, nil)
	nb.ok(thread("T-bob"), "receive_message", map[string]any{})
	nb.ok(thread("T-carol"), "receive_message", map[string]any{})
	ask := na.ok(nil, "ask", send("bob", "question?"))
	if out := nb.ok(thread("T-carol"), "receive_message", map[string]any{}); len(messageIDs(out)) != 0 {
		t.Fatalf("other thread pulled %v", out)
	}
	nb.fails(thread("T-carol"), "reply", map[string]any{"message_id": ask["message_id"], "text": "mine now"}, "invalid", ReasonNoSuchMessage)
	out := nb.ok(thread("T-bob"), "receive_message", map[string]any{})
	e := out["messages"].([]any)[0].(map[string]any)
	if e["message_id"] != ask["message_id"] || e["possibly_seen"] != nil || e["state"] != "delivered" {
		t.Fatalf("owner pull after another thread's calls: %v", out)
	}
}

func TestMailSignal_ThroughShimInjectorWrapper_ExpectCalledAfterMailboxedCommitAndNotForAccepted(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	w.codex.addThread("T-bob", "bob")
	w.codex.SetDefaultInject(adapter.Outcome{State: adapter.Mailboxed})
	na := w.node(alice.pid, w.lb, nil)
	sh := &signalHarness{}
	nb := w.node(1, w.lb, func(o *Options) {
		for i, h := range o.Harnesses {
			if h.Name() == adapter.Codex {
				sh.Harness = h
				o.Harnesses[i] = sh
			}
		}
	})
	st := func() *AgentState { return nb.sh.Agent(identity.AgentID("codex", "T-bob")) }
	nb.ok(thread("T-bob"), "receive_message", map[string]any{})
	var atSignal string
	sh.onCall = func(m msg.Message) {
		_, key, _ := st().inbound(m.MessageID)
		atSignal, _ = st().agent.Journal().Dedupe(key, m.MessageID)
	}
	ask := na.ok(nil, "ask", send("bob", "question?"))
	if atSignal != "mailboxed" {
		t.Fatalf("dedupe state at signal time %q", atSignal)
	}
	if len(sh.signals) != 1 || sh.signals[0] != ask["message_id"] {
		t.Fatalf("signals %v", sh.signals)
	}
	nb.ok(thread("T-bob"), "receive_message", map[string]any{})
	w.codex.SetDefaultInject(adapter.Outcome{State: adapter.Accepted})
	na.ok(nil, "ask", send("bob", "second?"))
	if len(sh.signals) != 1 {
		t.Fatalf("signals %v after a pull and an accepted send", sh.signals)
	}
}
