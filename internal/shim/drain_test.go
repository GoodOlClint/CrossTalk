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
