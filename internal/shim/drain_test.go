package shim

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/harness"
	"github.com/GoodOlClint/crosstalk/internal/harness/claude"
	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/msg"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

type blockedTransport struct {
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func newBlockedTransport() *blockedTransport {
	return &blockedTransport{entered: make(chan struct{}), release: make(chan struct{})}
}

func (b *blockedTransport) Send(context.Context, registry.Descriptor, registry.Descriptor, msg.Message, bool) (adapter.State, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return adapter.Accepted, nil
}

// sendInFlight starts a send from n and returns once it is blocked in tr.
func sendInFlight(t *testing.T, n *node, tr *blockedTransport, to string) <-chan map[string]any {
	t.Helper()
	res := make(chan map[string]any, 1)
	go func() {
		out := map[string]any{}
		r, err := n.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "send_message", Arguments: send(to, "in flight")})
		if err == nil && r.StructuredContent != nil {
			b, _ := json.Marshal(r.StructuredContent)
			json.Unmarshal(b, &out)
		}
		res <- out
	}()
	select {
	case <-tr.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("send never reached the transport")
	}
	return res
}

func finalOutbound(t *testing.T, w *world, agentID, messageID string) string {
	t.Helper()
	state := ""
	for _, r := range journalRecords(t, w, agentID) {
		if r.Type == journal.TypeOutbound && r.MessageID == messageID {
			state = r.State
		}
	}
	return state
}

func TestRetireWaitsForInFlightSendBeforeClosingJournal(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-1", "alice")
	bob := w.claude(4002, "sess-bob", "bob")
	tr := newBlockedTransport()
	na := w.node(alice.pid, tr, nil)
	nb := w.node(bob.pid, nil, nil)
	recv(nb, nil)
	recv(na, nil)
	oldID := alice.agentID()
	j := na.sh.Agent(oldID).Agent().Journal()

	res := sendInFlight(t, na, tr, "bob")
	w.become(alice, "sess-2", "alice")
	retired := make(chan struct{})
	go func() {
		na.srv.RetireSuperseded()
		close(retired)
	}()
	select {
	case <-retired:
		t.Errorf("retire finished with a send in flight")
	case <-time.After(100 * time.Millisecond):
	}
	w.become(alice, "sess-1", "alice")
	if isErr, out := na.call(nil, "receive_message", map[string]any{}); !isErr || out["reason"] != "agent_held" {
		t.Errorf("call for the retiring session started: %v %v", isErr, out)
	}
	w.become(alice, "sess-2", "alice")

	close(tr.release)
	out := <-res
	<-retired
	if out["state"] != "accepted" {
		t.Errorf("send result %v", out)
	}
	if s := finalOutbound(t, w, oldID, out["message_id"].(string)); s != "accepted" {
		t.Errorf("final outbound record %q, want accepted", s)
	}
	if err := j.Append(journal.Record{Type: journal.TypeClaim}); err == nil {
		t.Error("journal still open after retire")
	}
	if _, ok := w.descriptor(oldID); ok {
		t.Error("descriptor survives retire")
	}
}

func TestRetireBudgetKeepsJournalOpenForWedgedSend(t *testing.T) {
	old := HandlerCloseWait
	HandlerCloseWait = 100 * time.Millisecond
	t.Cleanup(func() { HandlerCloseWait = old })

	w := newWorld(t)
	alice := w.claude(4001, "sess-1", "alice")
	bob := w.claude(4002, "sess-bob", "bob")
	tr := newBlockedTransport()
	na := w.node(alice.pid, tr, nil)
	nb := w.node(bob.pid, nil, nil)
	recv(nb, nil)
	recv(na, nil)
	oldID := alice.agentID()
	j := na.sh.Agent(oldID).Agent().Journal()

	res := sendInFlight(t, na, tr, "bob")
	w.become(alice, "sess-2", "alice")
	start := time.Now()
	na.srv.RetireSuperseded()
	if el := time.Since(start); el > 2*time.Second {
		t.Errorf("retire took %v past a %v budget", el, HandlerCloseWait)
	}
	if _, ok := w.descriptor(oldID); ok {
		t.Error("descriptor kept after the budget")
	}
	g, err := registry.New(w.state).Acquire(registry.Request{AgentID: oldID, Harness: "claude", Cwd: "/w"})
	if !errors.Is(err, registry.ErrAgentHeld) {
		t.Errorf("agent lock after the budget: %v", err)
		if g != nil {
			g.Close()
		}
	}
	if out := recv(na, nil); out["notice"] != nil {
		t.Errorf("replacement notice %v", out["notice"])
	}
	if d, _ := w.descriptor(alice.agentID()); d.Name != "alice" {
		t.Errorf("name not released: %+v", d)
	}

	close(tr.release)
	out := <-res
	if out["state"] != "accepted" {
		t.Errorf("send result %v", out)
	}
	if s := finalOutbound(t, w, oldID, out["message_id"].(string)); s != "accepted" {
		t.Errorf("final outbound record %q, want accepted", s)
	}
	if err := j.Append(journal.Record{Type: journal.TypeClaim}); err != nil {
		t.Errorf("journal closed after the budget: %v", err)
	}
}

// wedgedInject is Claude with an Inject that ignores its context until released.
type wedgedInject struct {
	*claude.Harness
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (h *wedgedInject) Inject(context.Context, msg.Message, adapter.Delivery) (adapter.State, error) {
	h.once.Do(func() { close(h.entered) })
	<-h.release
	return adapter.Accepted, nil
}

func retireAsync(n *node) <-chan struct{} {
	retired := make(chan struct{})
	go func() {
		n.srv.RetireSuperseded()
		close(retired)
	}()
	return retired
}

func requirePending(t *testing.T, retired <-chan struct{}) {
	t.Helper()
	select {
	case <-retired:
		t.Errorf("retire finished with a worker in flight")
	case <-time.After(100 * time.Millisecond):
	}
}

type refusingTransport struct{}

func (refusingTransport) Send(context.Context, registry.Descriptor, registry.Descriptor, msg.Message, bool) (adapter.State, error) {
	return adapter.Unreachable, errors.New("refused")
}

func TestRetireWaitsForReplyRecoveryWorker(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	bob := w.claude(4002, "sess-1", "bob")
	na := w.node(alice.pid, w.lb, nil)
	first := w.node(bob.pid, refusingTransport{}, nil)
	w.lb.Attach(first.sh)
	recv(na, nil)
	recv(first, nil)
	oldID := bob.agentID()
	ask := na.ok(nil, "ask", send("bob", "question?"))
	reply := first.ok(nil, "reply", map[string]any{"message_id": ask["message_id"], "text": "answer"})
	if reply["state"] == "accepted" {
		t.Fatalf("first forward %v", reply)
	}
	replyID := reply["message_id"].(string)
	first.stop()

	tr := newBlockedTransport()
	nb := w.node(bob.pid, tr, nil)
	recv(nb, nil)
	select {
	case <-tr.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("recovery worker never reached the transport")
	}
	j := nb.sh.Agent(oldID).Agent().Journal()

	w.become(bob, "sess-2", "bob")
	retired := retireAsync(nb)
	requirePending(t, retired)
	close(tr.release)
	<-retired

	if s := finalOutbound(t, w, oldID, replyID); s != "accepted" {
		t.Errorf("final outbound record %q, want accepted", s)
	}
	state := ""
	for _, r := range journalRecords(t, w, oldID) {
		if r.Type == journal.TypeObligation && r.MessageID == replyID {
			state = r.State
		}
	}
	if state != journal.StateDelivered {
		t.Errorf("final obligation record %q, want delivered", state)
	}
	if err := j.Append(journal.Record{Type: journal.TypeClaim}); err == nil {
		t.Error("journal still open after retire")
	}
}

func TestRetireWaitsForDeadlineCallback(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-1", "alice")
	bob := w.claude(4002, "sess-bob", "bob")
	ch, err := claude.NewHarness(claude.HarnessOptions{SessionsDir: w.sessions, Alive: fakePID})
	if err != nil {
		t.Fatal(err)
	}
	h := &wedgedInject{Harness: ch, entered: make(chan struct{}), release: make(chan struct{})}
	na := w.node(alice.pid, w.lb, func(o *Options) { o.Harnesses = []harness.Harness{h} })
	nb := w.node(bob.pid, w.lb, nil)
	recv(nb, nil)
	recv(na, nil)
	oldID := alice.agentID()
	j := na.sh.Agent(oldID).Agent().Journal()
	ask := na.ok(nil, "ask", map[string]any{"to": "bob", "text": "q?", "deadline_seconds": 1})
	if ask["state"] != "accepted" {
		t.Fatalf("ask %v", ask)
	}
	select {
	case <-h.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("deadline callback never reached the harness")
	}

	w.become(alice, "sess-2", "alice")
	retired := retireAsync(na)
	requirePending(t, retired)
	close(h.release)
	<-retired

	final := ""
	for _, r := range journalRecords(t, w, oldID) {
		if r.Type == journal.TypeDedupe && r.Sender == PrincipalCrossTalk {
			final = r.State
		}
	}
	if final != "accepted" {
		t.Errorf("final notice record %q, want accepted", final)
	}
	if err := j.Append(journal.Record{Type: journal.TypeClaim}); err == nil {
		t.Error("journal still open after retire")
	}
}

func TestWorkerRefusedByDrainWritesNothing(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-1", "alice")
	na := w.node(alice.pid, nil, nil)
	recv(na, nil)
	oldID := alice.agentID()
	st := na.sh.Agent(oldID)
	w.become(alice, "sess-2", "alice")
	na.srv.RetireSuperseded()
	before := len(journalRecords(t, w, oldID))
	st.expire(msg.Message{MessageID: "m", DeadlineSeconds: 1})
	if after := len(journalRecords(t, w, oldID)); after != before {
		t.Errorf("refused worker wrote %d records", after-before)
	}
}

func TestBudgetKeepsJournalForWedgedDeadlineWorker(t *testing.T) {
	old := HandlerCloseWait
	HandlerCloseWait = 100 * time.Millisecond
	t.Cleanup(func() { HandlerCloseWait = old })

	w := newWorld(t)
	alice := w.claude(4001, "sess-1", "alice")
	ch, err := claude.NewHarness(claude.HarnessOptions{SessionsDir: w.sessions, Alive: fakePID})
	if err != nil {
		t.Fatal(err)
	}
	h := &wedgedInject{Harness: ch, entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() { close(h.release) })
	na := w.node(alice.pid, nil, func(o *Options) { o.Harnesses = []harness.Harness{h} })
	recv(na, nil)
	oldID := alice.agentID()
	st := na.sh.Agent(oldID)
	j := st.Agent().Journal()
	go st.expire(msg.Message{MessageID: "01a1a768-3393-71fb-879d-e2fc28901e01", ContextID: "01a1a768-3393-71fb-879d-e2fc28901e01", DeadlineSeconds: 1})
	select {
	case <-h.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("worker never reached the harness")
	}
	w.become(alice, "sess-2", "alice")
	na.srv.RetireSuperseded()
	if err := j.Append(journal.Record{Type: journal.TypeClaim}); err != nil {
		t.Errorf("journal closed under a wedged worker: %v", err)
	}
	_, err = registry.New(w.state).Acquire(registry.Request{AgentID: oldID, Harness: "claude", Cwd: "/w"})
	if !errors.Is(err, registry.ErrAgentHeld) {
		t.Errorf("agent lock after the budget: %v", err)
	}
}

func TestPreBindDrainRunsBeforePublish(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	w.codex.addThread("T-bob", "bob")
	bob := bobTarget("T-bob")
	id := storeEntry(t, w, time.Now(), alice, bob, time.Now().Add(-time.Minute), "waiting")
	var routeOpen, published, ran bool
	nb := w.node(1, w.lb, func(o *Options) {
		o.StartA2A = func(_ context.Context, st *AgentState) (A2AListener, error) {
			ran = true
			routeOpen = st.Receiver().RouteOpen(id)
			_, published = w.descriptor(bob.AgentID)
			return A2AListener{}, nil
		}
	})
	nb.ok(thread("T-bob"), "receive_message", map[string]any{})
	if !ran || !routeOpen || published {
		t.Fatalf("at bind time: hook ran %v, route open %v, descriptor published %v", ran, routeOpen, published)
	}
	if _, ok := w.descriptor(bob.AgentID); !ok {
		t.Fatal("descriptor never published")
	}
}
