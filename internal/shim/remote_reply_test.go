package shim

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/identity"
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
