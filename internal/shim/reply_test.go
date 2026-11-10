package shim

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/identity"
	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/mcpserver"
	"github.com/GoodOlClint/crosstalk/internal/msg"
	"github.com/GoodOlClint/crosstalk/internal/prebind"
	"github.com/GoodOlClint/crosstalk/internal/receiver"
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

func bobTarget(sessionRef string) prebind.Target {
	return prebind.Target{AgentID: identity.AgentID("codex", sessionRef), Harness: "codex", SessionRef: sessionRef}
}

func storeEntry(t *testing.T, w *world, now time.Time, from *fakeClaude, to prebind.Target, deliveredAt time.Time, text string) string {
	t.Helper()
	w.token(from.agentID())
	id := msg.NewUUIDv7()
	m := msg.Message{MessageID: id, ContextID: id, From: from.name, To: "bob", Kind: msg.Inform, Delivery: msg.Immediate, HopChain: []string{msg.Hop(from.agentID())}, SentAt: deliveredAt, Parts: []msg.Part{{Type: "text", Text: text}}}
	body, _ := json.Marshal(m)
	st := prebind.New(w.state, prebind.Options{Now: func() time.Time { return now }})
	e := prebind.Entry{AgentID: to.AgentID, Harness: to.Harness, SessionRef: to.SessionRef, Sender: from.agentID(), Message: string(body), DeliveredAt: deliveredAt, State: "accepted"}
	if err := st.Append(e); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestPreBindReplySucceedsAfterBind(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	w.codex.addThread("T-bob", "bob")
	w.token(alice.agentID())
	na := w.node(alice.pid, w.lb, nil)
	nb := w.node(1, w.lb, nil)

	sent := na.ok(nil, "send_message", send("bob", "fyi"))
	if sent["state"] != "accepted" {
		t.Fatalf("send %v", sent)
	}
	id := sent["message_id"].(string)

	nb.ok(thread("T-bob"), "receive_message", map[string]any{})
	res := nb.ok(thread("T-bob"), "reply", map[string]any{"message_id": id, "text": "answer"})
	if res["state"] != "accepted" {
		t.Fatalf("reply %v", res)
	}
	waitFor(t, "alice to receive the reply", func() bool { return len(alice.got()) == 1 })
	if !strings.Contains(alice.got()[0], "answer") {
		t.Fatalf("alice got %q", alice.got())
	}
	bobID := bobTarget("T-bob").AgentID
	st := nb.sh.Agent(bobID)
	if st.Receiver().RouteOpen(id) {
		t.Fatal("route still open after the reply")
	}
	nb.fails(thread("T-bob"), "reply", map[string]any{"message_id": id, "text": "other"}, "rejected", ReasonAlreadyReplied)
	if res := nb.ok(thread("T-bob"), "reply", map[string]any{"message_id": id, "text": "answer"}); res["state"] != "accepted" || len(alice.got()) != 1 {
		t.Fatalf("identical retry %v, alice got %d", res, len(alice.got()))
	}
}

func TestPreBindReplyDrainsLateEntry(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	w.codex.addThread("T-bob", "bob")
	na := w.node(alice.pid, w.lb, nil)
	nb := w.node(1, w.lb, nil)
	na.ok(nil, "receive_message", map[string]any{})
	nb.ok(thread("T-bob"), "receive_message", map[string]any{})

	id := storeEntry(t, w, time.Now(), alice, bobTarget("T-bob"), time.Now().Add(-time.Minute), "late")
	res := nb.ok(thread("T-bob"), "reply", map[string]any{"message_id": id, "text": "answer"})
	if res["state"] != "accepted" {
		t.Fatalf("reply %v", res)
	}
	waitFor(t, "alice to receive the reply", func() bool { return len(alice.got()) == 1 })
	nb.fails(thread("T-bob"), "reply", map[string]any{"message_id": msg.NewUUIDv7(), "text": "x"}, "invalid", ReasonNoSuchMessage)
}

func TestPreBindPartialImportRepairedOnRetry(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	w.codex.addThread("T-bob", "bob")
	na := w.node(alice.pid, w.lb, nil)
	nb := w.node(1, w.lb, nil)
	na.ok(nil, "receive_message", map[string]any{})
	nb.ok(thread("T-bob"), "receive_message", map[string]any{})
	bob := bobTarget("T-bob")
	st := nb.sh.Agent(bob.AgentID)

	delivered := time.Now().Add(-time.Minute)
	id := storeEntry(t, w, time.Now(), alice, bob, delivered, "half imported")
	m := msg.Message{MessageID: id, ContextID: id, From: "alice", To: "bob", Kind: msg.Inform, Delivery: msg.Immediate, HopChain: []string{msg.Hop(alice.agentID())}, SentAt: delivered, Parts: []msg.Part{{Type: "text", Text: "half imported"}}}
	body, _ := json.Marshal(m)
	if err := st.Agent().Journal().Append(journal.Record{Type: journal.TypeDedupe, Sender: alice.agentID(), MessageID: id, State: "accepted", Message: body, Imported: true}); err != nil {
		t.Fatal(err)
	}
	if !st.Receiver().ImportIncomplete(id) || st.Receiver().RouteOpen(id) {
		t.Fatal("setup: expected a dedupe record with no route")
	}

	res := nb.ok(thread("T-bob"), "reply", map[string]any{"message_id": id, "text": "answer"})
	if res["state"] != "accepted" {
		t.Fatalf("reply %v", res)
	}
	waitFor(t, "alice to receive the reply", func() bool { return len(alice.got()) == 1 })
	if st.Receiver().ImportIncomplete(id) || st.Receiver().RouteOpen(id) {
		t.Fatal("route open or incomplete after the reply")
	}
	rs := journalRecords(t, w, bob.AgentID)
	opens := 0
	for _, r := range rs {
		if r.Type == journal.TypeRoute && r.MessageID == id && r.State == journal.StateOpen {
			opens++
		}
	}
	if opens != 1 {
		t.Fatalf("%d open records for the route", opens)
	}
	nb.fails(thread("T-bob"), "reply", map[string]any{"message_id": id, "text": "again"}, "rejected", ReasonAlreadyReplied)
}

func TestPreBindEntryOlderThanTTLGivesPreBindHint(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	w.codex.addThread("T-bob", "bob")
	bound := time.Now().UTC()
	nb := w.node(1, w.lb, func(o *Options) { o.Now = func() time.Time { return bound } })
	stale := bound.Add(-2 * time.Hour)
	id := uuidV7At(stale)
	w.token(alice.agentID())
	m := msg.Message{MessageID: id, ContextID: id, From: "alice", To: "bob", Kind: msg.Inform, Delivery: msg.Immediate, HopChain: []string{msg.Hop(alice.agentID())}, SentAt: stale, Parts: []msg.Part{{Type: "text", Text: "old"}}}
	body, _ := json.Marshal(m)
	b := bobTarget("T-bob")
	e := prebind.Entry{AgentID: b.AgentID, Harness: b.Harness, SessionRef: b.SessionRef, Sender: alice.agentID(), Message: string(body), DeliveredAt: stale, State: "accepted"}
	if err := prebind.New(w.state, prebind.Options{Now: func() time.Time { return bound }}).Append(e); err != nil {
		t.Fatal(err)
	}
	nb.ok(thread("T-bob"), "receive_message", map[string]any{})
	_, out := nb.call(thread("T-bob"), "reply", map[string]any{"message_id": id, "text": "x"})
	if out["reason"] != ReasonNoSuchMessage || !strings.Contains(out["hint"].(string), "may predate this session's binding") {
		t.Fatalf("reply %v", out)
	}
	if rs := journalRecords(t, w, b.AgentID); slices.ContainsFunc(rs, func(r journal.Record) bool { return r.Type == journal.TypeRoute && r.MessageID == id }) {
		t.Fatal("an entry older than the TTL opened a route")
	}
}

func TestInboxCopyReplyIsRejectedInboxCopy(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	w.codex.addThread("T-bob", "bob")
	w.token(alice.agentID())
	na := w.node(alice.pid, w.lb, nil)
	nb := w.node(1, w.lb, nil)
	na.ok(nil, "receive_message", map[string]any{})
	nb.ok(thread("T-bob"), "receive_message", map[string]any{})
	st := nb.sh.Agent(bobTarget("T-bob").AgentID)

	for _, kind := range []msg.Kind{msg.Inform, msg.Ask} {
		id := msg.NewUUIDv7()
		m := msg.Message{MessageID: id, ContextID: id, From: "alice", To: "bob", Kind: kind, Delivery: msg.Immediate, SentAt: time.Now(), Parts: []msg.Part{{Type: "text", Text: "watching"}}}
		res := st.Receiver().Receive(context.Background(), receiver.Inbound{Ingress: receiver.A2A, Message: m, Sender: receiver.Sender{Verified: true, AgentID: alice.agentID(), Name: "alice", Role: "primary"}, Inbox: "coord", InboxCopy: true})
		if res.State != "accepted" && res.State != "queued" {
			t.Fatalf("%s copy: %+v", kind, res)
		}
		isErr, out := nb.call(thread("T-bob"), "reply", map[string]any{"message_id": id, "text": "answer"})
		hint, _ := out["hint"].(string)
		if !isErr || out["error"] != mcpserver.CodeRejected || out["reason"] != ReasonInboxCopy || !strings.Contains(hint, "responder") {
			t.Fatalf("%s copy reply: %v %v", kind, isErr, out)
		}
	}
	if len(alice.got()) != 0 {
		t.Fatalf("a reply to a copy reached the sender: %q", alice.got())
	}
}
