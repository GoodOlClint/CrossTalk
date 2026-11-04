package shim

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/identity"
	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/msg"
	"github.com/GoodOlClint/crosstalk/internal/receiver"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

type countingHook struct {
	mu    sync.Mutex
	calls int
	fail  int
}

func (h *countingHook) hook(context.Context, *AgentState, msg.Message, msg.Message) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls++
	if h.calls <= h.fail {
		return errors.New("task journal unavailable")
	}
	return nil
}

func (h *countingHook) n() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

func (h *countingHook) opt(o *Options) { o.OnReply = h.hook }

func obligationState(st *AgentState, replyID string) string {
	l, _ := st.agent.Journal().Latest(journal.Record{Type: journal.TypeObligation, MessageID: replyID})
	return l.State
}

func TestReplyCompletionFailureIsRetried(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	w.codex.addThread("T-bob", "bob")
	na := w.node(alice.pid, w.lb, nil)
	h := &countingHook{fail: 1}
	nb := w.node(1, w.lb, h.opt)
	nb.ok(thread("T-bob"), "receive_message", map[string]any{})
	askID := na.ok(nil, "ask", send("bob", "question?"))["message_id"].(string)
	st := nb.sh.Agent(identity.AgentID("codex", "T-bob"))
	args := map[string]any{"message_id": askID, "text": "answer"}

	first := nb.ok(thread("T-bob"), "reply", args)
	if first["state"] != string(adapter.Unknown) || first["reason"] != "task journal unavailable" || len(alice.got()) != 0 {
		t.Fatalf("failed completion: result %v, alice got %q", first, alice.got())
	}
	if s := obligationState(st, first["message_id"].(string)); s != journal.StatePending {
		t.Fatalf("obligation after failed completion %q", s)
	}
	if !st.rcv.RouteOpen(askID) {
		t.Fatal("route closed before the task completed")
	}

	second := nb.ok(thread("T-bob"), "reply", args)
	if second["state"] != "accepted" || second["message_id"] != first["message_id"] || h.n() != 2 {
		t.Fatalf("retry %v (first %v), hook calls %d", second, first, h.n())
	}
	third := nb.ok(thread("T-bob"), "reply", args)
	if third["state"] != "accepted" || h.n() != 2 {
		t.Fatalf("stored result %v, hook calls %d", third, h.n())
	}
	if len(alice.got()) != 1 || st.rcv.RouteOpen(askID) {
		t.Fatalf("alice got %q, route open %v", alice.got(), st.rcv.RouteOpen(askID))
	}
}

// publishedTransport is the world's Loopback that counts sends made before
// the sender's descriptor is published.
type publishedTransport struct {
	*Loopback
	w           *world
	unpublished *atomic.Int32
}

func (p publishedTransport) Send(ctx context.Context, from, to registry.Descriptor, m msg.Message, ri bool) (adapter.State, error) {
	if _, ok := p.w.descriptor(from.AgentID); !ok {
		p.unpublished.Add(1)
	}
	return p.Loopback.Send(ctx, from, to, m, ri)
}

func TestStartupReplayResumesEachStage(t *testing.T) {
	for _, stage := range []string{journal.StatePending, journal.StateCompleted, "released", "forwarded"} {
		t.Run(stage, func(t *testing.T) {
			w := newWorld(t)
			alice := w.claude(4001, "sess-alice", "alice")
			w.codex.addThread("T-bob", "bob")
			na := w.node(alice.pid, w.lb, nil)
			nb := w.node(1, w.lb, nil)
			nb.ok(thread("T-bob"), "receive_message", map[string]any{})
			askID := na.ok(nil, "ask", send("bob", "question?"))["message_id"].(string)
			bobID := identity.AgentID("codex", "T-bob")
			st := nb.sh.Agent(bobID)
			j := st.agent.Journal()

			orig, key, _ := st.inbound(askID)
			r := st.newMessage(orig.From, msg.Inform, msg.Immediate, "answer")
			r.InReplyTo, r.ContextID = &askID, orig.ContextID
			appendStage := func(s string, body bool) {
				if err := j.Append(st.obligation(r, key, s, body, "")); err != nil {
					t.Fatal(err)
				}
			}
			appendStage(journal.StatePending, true)
			if stage != journal.StatePending {
				appendStage(journal.StateCompleted, false)
			}
			if stage == "released" || stage == "forwarded" {
				st.rcv.CloseRoute(askID)
				appendStage("released", false)
			}
			if stage == "forwarded" {
				fwd := r
				fwd.To = "alice"
				if s, err := w.lb.Send(context.Background(), st.agent.Descriptor(), registry.Descriptor{AgentID: alice.agentID()}, fwd, true); s != adapter.Accepted || err != nil {
					t.Fatalf("setup forward %v %v", s, err)
				}
				if err := j.Append(journal.Record{Type: journal.TypeOutbound, MessageID: r.MessageID, To: alice.agentID(), Kind: string(msg.Inform), State: string(adapter.Accepted), InReplyTo: askID, ContextID: r.ContextID}); err != nil {
					t.Fatal(err)
				}
			}
			nb.stop()

			h := &countingHook{}
			var unpublished atomic.Int32
			nb2 := w.node(1, publishedTransport{Loopback: w.lb, w: w, unpublished: &unpublished}, h.opt)
			w.lb.Attach(nb2.sh)
			nb2.ok(thread("T-bob"), "receive_message", map[string]any{})
			st2 := nb2.sh.Agent(bobID)
			waitFor(t, "obligation delivered", func() bool { return obligationState(st2, r.MessageID) == journal.StateDelivered })

			wantHooks := 0
			if stage == journal.StatePending {
				wantHooks = 1
			}
			if h.n() != wantHooks || len(alice.got()) != 1 || st2.rcv.RouteOpen(askID) || unpublished.Load() != 0 {
				t.Fatalf("hook calls %d (want %d), alice got %q, route open %v, sends before publish %d", h.n(), wantHooks, alice.got(), st2.rcv.RouteOpen(askID), unpublished.Load())
			}
		})
	}
}

func TestStartupReplayCompletesLocalUserObligation(t *testing.T) {
	w := newWorld(t)
	w.codex.addThread("T-bob", "bob")
	nb := w.node(1, nil, nil)
	nb.ok(thread("T-bob"), "receive_message", map[string]any{})
	bobID := identity.AgentID("codex", "T-bob")
	st := nb.sh.Agent(bobID)
	askID := msg.NewUUIDv7()
	in := receiver.Inbound{Ingress: receiver.A2A, Sender: receiver.Sender{Verified: true, Principal: receiver.PrincipalLocalUser}, Message: msg.Message{MessageID: askID, Kind: msg.Ask, Parts: []msg.Part{{Type: "text", Text: "question?"}}}}
	if res := st.Receive(context.Background(), in); res.State != adapter.Accepted {
		t.Fatalf("local-user ask %+v", res)
	}
	orig, key, _ := st.inbound(askID)
	r := st.newMessage(orig.From, msg.Inform, msg.Immediate, "answer")
	r.InReplyTo, r.ContextID = &askID, orig.ContextID
	if err := st.agent.Journal().Append(st.obligation(r, key, journal.StatePending, true, "")); err != nil {
		t.Fatal(err)
	}
	nb.stop()

	h := &countingHook{}
	nb2 := w.node(1, nil, h.opt)
	nb2.ok(thread("T-bob"), "receive_message", map[string]any{})
	st2 := nb2.sh.Agent(bobID)
	waitFor(t, "local-user obligation delivered", func() bool { return obligationState(st2, r.MessageID) == journal.StateDelivered })
	if h.n() != 1 {
		t.Fatalf("hook calls %d", h.n())
	}
}
