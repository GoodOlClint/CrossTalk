package shim

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/identity"
	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/msg"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

type inboxWorld struct {
	w      *world
	alice  *fakeClaude
	na, nb *node
	bob    string
	carol  string
}

// newInboxWorld has alice as sender and two pooled Codex threads on one shim,
// bob (owner, attached first) and carol (granted, attached second and so the
// responder), both in inbox coord.
func newInboxWorld(t *testing.T) *inboxWorld {
	t.Helper()
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	w.codex.addThread("T-bob", "bob")
	w.codex.addThread("T-carol", "carol")
	iw := &inboxWorld{w: w, alice: alice, bob: identity.AgentID("codex", "T-bob"), carol: identity.AgentID("codex", "T-carol")}
	iw.na = w.node(alice.pid, w.lb, nil)
	iw.nb = w.node(1, w.lb, nil)
	iw.na.ok(nil, "receive_message", map[string]any{})
	iw.nb.ok(thread("T-bob"), "receive_message", map[string]any{})
	iw.nb.ok(thread("T-carol"), "receive_message", map[string]any{})
	iw.nb.ok(thread("T-bob"), "attach", map[string]any{"inbox": "coord"})
	iw.nb.ok(thread("T-bob"), "grant", map[string]any{"inbox": "coord", "to": iw.carol})
	iw.nb.ok(thread("T-carol"), "attach", map[string]any{"inbox": "coord"})
	return iw
}

func (iw *inboxWorld) allWritesTo(ref string) []adapter.Call {
	var out []adapter.Call
	for _, c := range iw.w.codex.Writes() {
		if c.SessionRef == ref {
			out = append(out, c)
		}
	}
	return out
}

// writesTo leaves out the membership notices the setup's attaches produced.
func (iw *inboxWorld) writesTo(ref string) []adapter.Call {
	var out []adapter.Call
	for _, c := range iw.allWritesTo(ref) {
		if !strings.HasPrefix(c.Message.Text(), "Inbox coord: session ") {
			out = append(out, c)
		}
	}
	return out
}

func TestInboxNewestMemberResponds(t *testing.T) {
	iw := newInboxWorld(t)
	res := iw.na.ok(nil, "send_message", send("inbox:coord", "status?"))
	if res["state"] != "accepted" || res["inbox"] != "coord" || res["responder"] != iw.carol {
		t.Fatalf("result %v", res)
	}
	got := iw.writesTo("T-carol")
	if len(got) != 1 || !strings.Contains(got[0].Message.Parts[0].Text, "status?") {
		t.Fatalf("carol writes %+v", got)
	}
	if len(iw.writesTo("T-bob")) != 1 {
		t.Fatalf("bob writes %+v, want one copy", iw.writesTo("T-bob"))
	}
	st := iw.na.sh.Agent(iw.alice.agentID())
	o, ok := st.agent.Journal().Latest(journal.Record{Type: journal.TypeOutbound, MessageID: res["message_id"].(string)})
	if !ok || o.To != iw.carol || o.Kind != "inform" {
		t.Fatalf("outbound %+v", o)
	}
}

func TestInboxAskOneReply(t *testing.T) {
	iw := newInboxWorld(t)
	res := iw.na.ok(nil, "ask", map[string]any{"to": "inbox:coord", "text": "which branch?", "deadline_seconds": 600})
	id := res["message_id"].(string)
	if res["inbox"] != "coord" || res["responder"] != iw.carol {
		t.Fatalf("result %v", res)
	}
	st := iw.na.sh.Agent(iw.alice.agentID())
	o, ok := st.agent.Journal().Latest(journal.Record{Type: journal.TypeOutbound, MessageID: id})
	if !ok || o.To != iw.carol || o.Kind != "ask" || o.State != "accepted" {
		t.Fatalf("outbound %+v", o)
	}
	st.mu.Lock()
	armed := st.deadline[id] != nil
	st.mu.Unlock()
	if !armed {
		t.Fatal("ask deadline not armed")
	}
	iw.nb.fails(thread("T-bob"), "reply", map[string]any{"message_id": id, "text": "me?"}, "rejected", ReasonInboxCopy)
	if rep := iw.nb.ok(thread("T-carol"), "reply", map[string]any{"message_id": id, "text": "main"}); rep["state"] != "accepted" {
		t.Fatalf("reply %v", rep)
	}
	if got := iw.alice.got(); len(got) != 1 || !strings.Contains(got[0], "main") {
		t.Fatalf("alice got %q", got)
	}
	if !st.replied(id) {
		t.Fatal("responder's reply not recognized")
	}
}

func TestInboxCommandOnlyToResponder(t *testing.T) {
	iw := newInboxWorld(t)
	res := iw.na.ok(nil, "send_message", map[string]any{"to": "inbox:coord", "text": "run tests", "kind": "command"})
	if res["responder"] != iw.carol || res["state"] == "rejected" {
		t.Fatalf("result %v", res)
	}
	if got := iw.writesTo("T-carol"); len(got) != 1 || got[0].Message.Kind != "command" {
		t.Fatalf("carol writes %+v", got)
	}
	if len(iw.writesTo("T-bob")) != 0 {
		t.Fatalf("a command reached a watcher: %+v", iw.writesTo("T-bob"))
	}
}

func TestInboxCodexThreadsIndependent(t *testing.T) {
	iw := newInboxWorld(t)
	for _, id := range []string{iw.bob, iw.carol} {
		d, ok := iw.w.descriptor(id)
		if !ok || len(d.Inboxes) != 1 || d.Inboxes[0].Name != "coord" {
			t.Fatalf("descriptor %s: %+v", id, d.Inboxes)
		}
	}
	iw.nb.ok(thread("T-carol"), "detach", map[string]any{"inbox": "coord"})
	if d, _ := iw.w.descriptor(iw.carol); len(d.Inboxes) != 0 {
		t.Fatalf("carol still attached: %+v", d.Inboxes)
	}
	if d, _ := iw.w.descriptor(iw.bob); len(d.Inboxes) != 1 {
		t.Fatalf("bob's membership changed by carol's detach: %+v", d.Inboxes)
	}
	res := iw.na.ok(nil, "send_message", send("inbox:coord", "now?"))
	if res["responder"] != iw.bob || len(iw.writesTo("T-bob")) != 1 || len(iw.writesTo("T-carol")) != 0 {
		t.Fatalf("result %v bob %d carol %d", res, len(iw.writesTo("T-bob")), len(iw.writesTo("T-carol")))
	}
}

func TestInboxNudgeCountsInboxAsOnePeer(t *testing.T) {
	iw := newInboxWorld(t)
	var last map[string]any
	for i := 1; i <= NudgeFirst; i++ {
		if i == NudgeFirst/2 {
			iw.nb.ok(thread("T-carol"), "detach", map[string]any{"inbox": "coord"})
		}
		last = iw.na.ok(nil, "send_message", send("inbox:coord", "ping"))
		if h, _ := last["hint"].(string); i < NudgeFirst && strings.Contains(h, "You have sent this peer") {
			t.Fatalf("nudge at send %d: %v", i, h)
		}
	}
	if h, _ := last["hint"].(string); !strings.Contains(h, "sent this peer 30 messages") {
		t.Fatalf("send %d hint %q, want the nudge although the responder changed", NudgeFirst, h)
	}
}

func copiesOf(t *testing.T, res map[string]any) map[string]string {
	t.Helper()
	out := map[string]string{}
	list, _ := res["copies"].([]any)
	for _, c := range list {
		row := c.(map[string]any)
		out[row["agent_id"].(string)], _ = row["state"].(string)
	}
	return out
}

func TestInboxFanOutResponderGetsKindOthersGetCopies(t *testing.T) {
	iw := newInboxWorld(t)
	res := iw.na.ok(nil, "ask", map[string]any{"to": "inbox:coord", "text": "which branch?"})
	id := res["message_id"].(string)
	if c := copiesOf(t, res); len(c) != 1 || c[iw.bob] != "accepted" || res["responder"] != iw.carol {
		t.Fatalf("result %v", res)
	}
	resp, copy := iw.writesTo("T-carol"), iw.writesTo("T-bob")
	if len(resp) != 1 || len(copy) != 1 {
		t.Fatalf("carol %d bob %d writes", len(resp), len(copy))
	}
	for _, c := range []adapter.Call{resp[0], copy[0]} {
		if c.Message.MessageID != id || c.Message.Kind != msg.Ask || c.Delivery.Inbox != "coord" {
			t.Fatalf("delivered %+v inbox %q", c.Message, c.Delivery.Inbox)
		}
	}
	iw.nb.fails(thread("T-bob"), "reply", map[string]any{"message_id": id, "text": "me?"}, "rejected", ReasonInboxCopy)
	if rep := iw.nb.ok(thread("T-carol"), "reply", map[string]any{"message_id": id, "text": "main"}); rep["state"] != "accepted" {
		t.Fatalf("reply %v", rep)
	}

	res = iw.na.ok(nil, "send_message", map[string]any{"to": "inbox:coord", "text": "run tests", "kind": "command"})
	if len(copiesOf(t, res)) != 0 || len(iw.writesTo("T-bob")) != 1 {
		t.Fatalf("a command was copied: %v", res)
	}
}

func TestInboxCopyLegNeverOverwritesResponderLeg(t *testing.T) {
	iw := newInboxWorld(t)
	res := iw.na.ok(nil, "ask", map[string]any{"to": "inbox:coord", "text": "which branch?"})
	id := res["message_id"].(string)
	st := iw.na.sh.Agent(iw.alice.agentID())
	j := st.agent.Journal()
	o, ok := j.Latest(journal.Record{Type: journal.TypeOutbound, MessageID: id})
	if !ok || o.To != iw.carol || o.Kind != "ask" || o.State != "accepted" {
		t.Fatalf("outbound %+v", o)
	}
	l, ok := j.Latest(journal.Record{Type: journal.TypeLeg, MessageID: id, To: iw.bob})
	if !ok || l.State != "accepted" || l.Kind != "ask" {
		t.Fatalf("copy leg %+v", l)
	}
	iw.na.stop()
	na := iw.w.node(iw.alice.pid, iw.w.lb, nil)
	na.ok(nil, "receive_message", map[string]any{})
	if rep := iw.nb.ok(thread("T-carol"), "reply", map[string]any{"message_id": id, "text": "main"}); rep["state"] != "accepted" {
		t.Fatalf("reply %v", rep)
	}
	if !na.sh.Agent(iw.alice.agentID()).replied(id) {
		t.Fatal("responder's reply not recognized after the sender restarted")
	}
}

type recordingTransport struct {
	*Loopback
	mu    sync.Mutex
	sends []recordedSend
}

type recordedSend struct {
	to   string
	copy bool
}

func (r *recordingTransport) Send(ctx context.Context, from, to registry.Descriptor, m msg.Message, ri bool) (adapter.State, error) {
	r.mu.Lock()
	r.sends = append(r.sends, recordedSend{to.AgentID, m.InboxCopy})
	r.mu.Unlock()
	return r.Loopback.Send(ctx, from, to, m, ri)
}

func TestInboxMixedBuildReceiverNeverGetsReplyableCopy(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	w.codex.addThread("T-bob", "bob")
	w.codex.addThread("T-carol", "carol")
	w.codex.addThread("T-dave", "dave")
	tr := &recordingTransport{Loopback: w.lb}
	na := w.node(alice.pid, tr, nil)
	nb := w.node(1, tr, nil)
	tr.Attach(na.sh, nb.sh)
	na.ok(nil, "receive_message", map[string]any{})
	for _, id := range []string{"T-bob", "T-carol", "T-dave"} {
		nb.ok(thread(id), "receive_message", map[string]any{})
	}
	carol := identity.AgentID("codex", "T-carol")
	nb.ok(thread("T-bob"), "attach", map[string]any{"inbox": "coord"})
	nb.ok(thread("T-bob"), "grant", map[string]any{"inbox": "coord", "to": carol})
	nb.ok(thread("T-carol"), "attach", map[string]any{"inbox": "coord"})

	tr.mu.Lock()
	tr.sends = nil
	tr.mu.Unlock()
	res := na.ok(nil, "ask", map[string]any{"to": "inbox:coord", "text": "q?"})
	dave := identity.AgentID("codex", "T-dave")
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if len(tr.sends) != 2 || res["responder"] != carol {
		t.Fatalf("sends %+v result %v", tr.sends, res)
	}
	for _, s := range tr.sends {
		if s.to == dave || s.copy != (s.to != carol) {
			t.Fatalf("send %+v: copies go to members only and the responder's is no copy", s)
		}
	}
	for _, c := range w.codex.Writes() {
		if c.SessionRef == "T-dave" {
			t.Fatalf("a registered non-member got %+v", c.Message)
		}
	}
}
