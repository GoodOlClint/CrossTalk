package shim

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/identity"
	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/msg"
	"github.com/GoodOlClint/crosstalk/internal/receiver"
	"github.com/GoodOlClint/crosstalk/internal/registry"
	"github.com/GoodOlClint/crosstalk/internal/remote"
)

type countingRemote struct{ n atomic.Int32 }

func (c *countingRemote) Send(context.Context, registry.Descriptor, registry.Descriptor, msg.Message, bool) (adapter.State, error) {
	c.n.Add(1)
	return adapter.Accepted, nil
}

func (c *countingRemote) SendRemote(context.Context, registry.Descriptor, remote.Host, remote.Peer, string, msg.Message) (string, adapter.State, error) {
	c.n.Add(1)
	return "", adapter.Accepted, nil
}

func TestNativeReplyToRemoteAskBounced(t *testing.T) {
	w := newWorld(t)
	w.codex.addThread("T-bob", "bob")
	tr := &countingRemote{}
	nb := w.node(1, tr, func(o *Options) {
		o.OnReply = func(context.Context, *AgentState, msg.Message, msg.Message) error { return nil }
	})
	nb.ok(thread("T-bob"), "receive_message", map[string]any{})
	id := identity.AgentID("codex", "T-bob")
	st := nb.sh.Agent(id)
	receiveRemote(t, st, msg.Ask, "question?")

	m := msg.NewUUIDv7()
	res := st.rcv.Receive(context.Background(), receiver.Inbound{Ingress: receiver.UDS, Native: true,
		Sender:  receiver.Sender{Verified: true, AgentID: id, Name: "bob", Role: registry.RolePrimary, Harness: "codex"},
		Message: msg.Message{MessageID: m, ContextID: m, To: "alice@desk", Kind: msg.Inform, Delivery: msg.Immediate, Parts: []msg.Part{{Type: "text", Text: "42"}}}})
	if res.State != adapter.Bounced {
		t.Fatalf("native frame answering a remote ask: %+v, want bounced", res)
	}
	if n := tr.n.Load(); n != 0 {
		t.Fatalf("%d requests to the peer for a bounced native reply, want 0", n)
	}
}

func TestEndReplyReportsJournalFailure(t *testing.T) {
	w := newWorld(t)
	w.codex.addThread("T-bob", "bob")
	nb := w.node(1, &countingRemote{}, nil)
	nb.ok(thread("T-bob"), "receive_message", map[string]any{})
	st := nb.sh.Agent(identity.AgentID("codex", "T-bob"))
	irt := msg.NewUUIDv7()
	r := st.newMessage("alice@desk", msg.Inform, msg.Immediate, "42")
	r.InReplyTo = &irt
	st.agent.Journal().Close()
	res := st.endReply(r, remoteKey, "", ObligationRejected, ReasonNoSuchAgent)
	if res.State != adapter.Unknown || res.Reason != receiver.ReasonJournal || res.MessageID != r.MessageID {
		t.Fatalf("endReply with the journal closed: %+v, want unknown/%s", res, receiver.ReasonJournal)
	}
}

func TestReplyLimitAnchoredOnReceiptNotSentAt(t *testing.T) {
	w := newWorld(t)
	w.codex.addThread("T-bob", "bob")
	nb := w.node(1, &countingRemote{}, func(o *Options) {
		o.OnReply = func(context.Context, *AgentState, msg.Message, msg.Message) error { return nil }
	})
	nb.ok(thread("T-bob"), "receive_message", map[string]any{})
	st := nb.sh.Agent(identity.AgentID("codex", "T-bob"))
	old := time.Now().Add(-2 * time.Hour)
	id := uuidV7At(old)
	ask := msg.Message{MessageID: id, ContextID: id, From: "x", To: "bob", Kind: msg.Ask, Delivery: msg.Immediate, DeadlineSeconds: 300,
		SentAt: old, Parts: []msg.Part{{Type: "text", Text: "question?"}}}
	if res := st.rcv.Receive(context.Background(), receiver.Inbound{Ingress: receiver.A2A, Message: ask, Sender: remoteSnd}); res.State != adapter.Accepted {
		t.Fatalf("ask: %+v", res)
	}
	isErr, out := nb.call(thread("T-bob"), "reply", map[string]any{"message_id": id, "text": "42"})
	if out["reason"] != ReasonRemoteReplyPending || (isErr && out["error"] != string(adapter.Unreachable)) {
		t.Fatalf("reply to an ask whose SentAt and message_id time are 2 h old: %v, want pending (limit runs from the receipt time on this host's clock)", out)
	}
	rs, _ := st.agent.Journal().Records()
	for _, r := range rs {
		if r.Type == journal.TypeObligation && r.To == remoteKey {
			if l, _ := st.agent.Journal().Latest(r); l.State == ObligationUnreachable {
				t.Fatalf("obligation ended %+v", l)
			}
		}
	}
}

func TestRemoteReplyIntoMailbox_ExpectDeliveredNotRetried(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "remote")
	if _, err := remote.LoadOrCreateHost(dir); err != nil {
		t.Fatal(err)
	}
	if err := remote.NewPeers(dir).Upsert(remote.Peer{Alias: "desk", Address: "127.0.0.1:7745", Fingerprint: remoteHost}); err != nil {
		t.Fatal(err)
	}
	w := newWorld(t)
	w.codex.addThread("T-bob", "bob")
	tr := &stateTransport{state: adapter.Mailboxed}
	nb := w.node(1, tr, func(o *Options) {
		o.RemoteConfigDir = dir
		o.OnReply = func(context.Context, *AgentState, msg.Message, msg.Message) error { return nil }
	})
	nb.ok(thread("T-bob"), "receive_message", map[string]any{})
	st := nb.sh.Agent(identity.AgentID("codex", "T-bob"))
	ask := receiveRemote(t, st, msg.Ask, "question?")
	args := map[string]any{"message_id": ask.MessageID, "text": "answer"}
	out := nb.ok(thread("T-bob"), "reply", args)
	if out["state"] != string(adapter.Mailboxed) || out["reason"] == ReasonRemoteReplyPending {
		t.Fatalf("reply %v, want mailboxed and handed off", out)
	}
	if l, _ := st.agent.Journal().Latest(journal.Record{Type: journal.TypeObligation, MessageID: out["message_id"].(string)}); l.State != journal.StateDelivered {
		t.Fatalf("obligation %+v, want delivered", l)
	}
	nb.ok(thread("T-bob"), "reply", args)
	if n := tr.n.Load(); n != 1 {
		t.Fatalf("SendRemote called %d times, want 1", n)
	}
}
