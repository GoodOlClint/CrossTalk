package shim

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/identity"
	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/msg"
	"github.com/GoodOlClint/crosstalk/internal/receiver"
)

func answer(askID, contextID, text string) msg.Message {
	return msg.Message{MessageID: msg.NewUUIDv7(), InReplyTo: &askID, ContextID: contextID, Kind: msg.Inform, Parts: []msg.Part{{Type: "text", Text: text}}}
}

func TestRejectedMessageDoesNotSuppressDeadline(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	w.codex.addThread("T-bob", "bob")
	na := w.node(alice.pid, w.lb, nil)
	nb := w.node(1, w.lb, nil)
	nb.ok(thread("T-bob"), "receive_message", map[string]any{})
	askID := na.ok(nil, "ask", send("bob", "question?"))["message_id"].(string)
	st := na.sh.Agent(alice.agentID())
	bob := receiver.Sender{Verified: true, AgentID: identity.AgentID("codex", "T-bob"), Name: "bob"}

	cmd := answer(askID, askID, "not authorized")
	cmd.Kind = msg.Command
	if res := st.Receive(context.Background(), receiver.Inbound{Ingress: receiver.A2A, Sender: receiver.Sender{Verified: true, Principal: receiver.PrincipalLocalUser}, Message: cmd}); res.State != adapter.Rejected {
		t.Fatalf("setup did not reject: %+v", res)
	}
	big := answer(askID, askID, strings.Repeat("x", receiver.DefaultLimits.MaxMessageBytes+1))
	if res := st.Receive(context.Background(), receiver.Inbound{Ingress: receiver.A2A, Sender: bob, Message: big}); res.State != adapter.Rejected {
		t.Fatalf("oversized reply from the asked peer not rejected: %+v", res)
	}
	if st.replied(askID) {
		t.Fatalf("a policy- or admission-rejected message counts as a reply to %s", askID)
	}
}

func TestOnlyAskedPeerInAskContextSuppressesDeadline(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	w.codex.addThread("T-bob", "bob")
	na := w.node(alice.pid, w.lb, nil)
	nb := w.node(1, w.lb, nil)
	nb.ok(thread("T-bob"), "receive_message", map[string]any{})
	askID := na.ok(nil, "ask", send("bob", "question?"))["message_id"].(string)
	st := na.sh.Agent(alice.agentID())
	carol := receiver.Sender{Verified: true, AgentID: "c3c3c3c3c3c3c3c3", Name: "carol"}
	bob := receiver.Sender{Verified: true, AgentID: identity.AgentID("codex", "T-bob"), Name: "bob"}

	if res := st.Receive(context.Background(), receiver.Inbound{Ingress: receiver.A2A, Sender: carol, Message: answer(askID, askID, "me neither")}); res.State != adapter.Accepted {
		t.Fatalf("carol's message %+v", res)
	}
	if res := st.Receive(context.Background(), receiver.Inbound{Ingress: receiver.A2A, Sender: bob, Message: answer(askID, "other-context", "elsewhere")}); res.State != adapter.Accepted {
		t.Fatalf("bob's other-context message %+v", res)
	}
	if st.replied(askID) {
		t.Fatal("a reply from the wrong peer or context counts")
	}
	nb.ok(thread("T-bob"), "reply", map[string]any{"message_id": askID, "text": "answer"})
	if !st.replied(askID) {
		t.Fatal("bob's reply does not count")
	}
}

func TestLegacyReplyBodyBecomesReceiptOnReplay(t *testing.T) {
	run := func(t *testing.T, peer string) (*fakeClaude, []journal.Record) {
		w := newWorld(t)
		alice := w.claude(4001, "sess-alice", "alice")
		w.codex.addThread("T-bob", "bob")
		t0 := time.Date(2026, 11, 3, 13, 0, 0, 0, time.UTC)
		na := w.node(alice.pid, pendingTransport{taskID: "task-far"}, func(o *Options) { o.Now = fakeClock(t0) })
		nb := w.node(1, w.lb, nil)
		nb.ok(thread("T-bob"), "receive_message", map[string]any{})
		askID := na.ok(nil, "ask", map[string]any{"to": "bob", "text": "slow?", "deadline_seconds": 60})["message_id"].(string)
		if peer == "" {
			peer = identity.AgentID("codex", "T-bob")
		}
		body, _ := json.Marshal(answer(askID, askID, "legacy"))
		if err := na.sh.Agent(alice.agentID()).agent.Journal().Append(journal.Record{Type: journal.TypeDedupe, Sender: peer, MessageID: msg.NewUUIDv7(), State: string(adapter.Accepted), Message: body}); err != nil {
			t.Fatal(err)
		}
		na.stop()
		n := w.node(alice.pid, nil, func(o *Options) { o.Now = fakeClock(t0.Add(2 * time.Minute)) })
		n.ok(nil, "receive_message", map[string]any{})
		time.Sleep(200 * time.Millisecond)
		return alice, journalRecords(t, w, alice.agentID())
	}
	receipts := func(rs []journal.Record) int {
		c := 0
		for _, r := range rs {
			if r.Type == journal.TypeReceipt {
				c++
			}
		}
		return c
	}

	t.Run("asked peer", func(t *testing.T) {
		alice, rs := run(t, "")
		if len(alice.got()) != 0 || receipts(rs) != 1 {
			t.Fatalf("delivered %q, receipts %d", alice.got(), receipts(rs))
		}
	})
	t.Run("other peer", func(t *testing.T) {
		alice, rs := run(t, "c3c3c3c3c3c3c3c3")
		waitFor(t, "expiry", func() bool { return len(alice.got()) == 1 })
		if receipts(rs) != 0 {
			t.Fatalf("receipts %d", receipts(rs))
		}
	})
}
