package shim

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/identity"
	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/mcpserver"
	"github.com/GoodOlClint/crosstalk/internal/msg"
	"github.com/GoodOlClint/crosstalk/internal/receiver"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

var (
	remoteHost  = strings.Repeat("a", 64)
	remoteAgent = "e1e1e1e1e1e1e1e1"
	remoteKey   = receiver.RemoteKey(remoteHost, remoteAgent)
	remoteSnd   = receiver.Sender{Verified: true, Host: remoteHost, HostAlias: "desk", AgentID: remoteAgent, Name: "alice@desk", Role: registry.RolePrimary, Harness: "claude", Principal: remoteKey}
)

func receiveRemote(t *testing.T, st *AgentState, kind msg.Kind, text string) msg.Message {
	t.Helper()
	id := msg.NewUUIDv7()
	m := msg.Message{MessageID: id, ContextID: id, From: "x", To: "bob", Kind: kind, Delivery: msg.Immediate, SentAt: time.Now(), Parts: []msg.Part{{Type: "text", Text: text}}}
	if res := st.rcv.Receive(context.Background(), receiver.Inbound{Ingress: receiver.A2A, Message: m, Sender: remoteSnd}); res.State != adapter.Accepted {
		t.Fatalf("remote %s: %+v", kind, res)
	}
	return m
}

func TestRemoteReplyNeverMarkedDeliveredLocally(t *testing.T) {
	w := newWorld(t)
	w.codex.addThread("T-bob", "bob")
	hooks := 0
	nb := w.node(1, w.lb, func(o *Options) {
		o.OnReply = func(context.Context, *AgentState, msg.Message, msg.Message) error { hooks++; return nil }
	})
	nb.ok(thread("T-bob"), "receive_message", map[string]any{})
	st := nb.sh.Agent(identity.AgentID("codex", "T-bob"))
	ask := receiveRemote(t, st, msg.Ask, "question?")

	args := map[string]any{"message_id": ask.MessageID, "text": "answer"}
	for i := 0; i < 2; i++ {
		isErr, out := nb.call(thread("T-bob"), "reply", args)
		if isErr && out["error"] != string(adapter.Unreachable) {
			t.Fatalf("attempt %d: error result %v", i, out)
		}
		if out["state"] == "accepted" || out["state"] == "queued" {
			t.Fatalf("attempt %d: reply to a remote sender reported %v with no gateway answer", i, out["state"])
		}
		if out["state"] != string(adapter.Unreachable) && out["error"] != string(adapter.Unreachable) || out["reason"] != ReasonRemoteReplyPending {
			t.Fatalf("attempt %d: %v, want unreachable/%s", i, out, ReasonRemoteReplyPending)
		}
	}
	if hooks != 1 {
		t.Fatalf("OnReply ran %d times, want 1", hooks)
	}
	rs, _ := st.agent.Journal().Records()
	var obligation string
	for _, r := range rs {
		if r.Type == journal.TypeObligation && r.To == remoteKey {
			l, _ := st.agent.Journal().Latest(r)
			obligation = l.State
		}
		if r.Type == journal.TypeOutbound && r.InReplyTo == ask.MessageID {
			t.Fatalf("an outbound record exists for a reply that was never sent: %+v", r)
		}
	}
	if obligation == "" || obligation == journal.StateDelivered || obligation == journal.StateClosed {
		t.Fatalf("obligation state %q, want pending in the journal", obligation)
	}
}

func TestRemoteInformRouteExpires(t *testing.T) {
	w := newWorld(t)
	w.codex.addThread("T-bob", "bob")
	var mu sync.Mutex
	now := time.Date(2026, 11, 3, 13, 0, 0, 0, time.UTC)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
	nb := w.node(1, w.lb, func(o *Options) {
		o.Now = clock
		o.OnReply = func(context.Context, *AgentState, msg.Message, msg.Message) error { return nil }
	})
	nb.ok(thread("T-bob"), "receive_message", map[string]any{})
	st := nb.sh.Agent(identity.AgentID("codex", "T-bob"))

	old := receiveRemote(t, st, msg.Inform, "fyi")
	advance(61 * time.Minute)
	fresh := receiveRemote(t, st, msg.Inform, "fyi again")
	nb.fails(thread("T-bob"), "reply", map[string]any{"message_id": old.MessageID, "text": "late"}, mcpserver.CodeInvalid, ReasonNoSuchMessage)
	isErr, out := nb.call(thread("T-bob"), "reply", map[string]any{"message_id": fresh.MessageID, "text": "thanks"})
	if out["reason"] != ReasonRemoteReplyPending || (isErr && out["error"] != string(adapter.Unreachable)) {
		t.Fatalf("reply to a fresh remote inform: %v", out)
	}
}
