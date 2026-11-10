package shim

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoodOlClint/crosstalk/internal/identity"
	"github.com/GoodOlClint/crosstalk/internal/msg"
)

func (iw *inboxWorld) lastWriteTo(t *testing.T, ref string, want int) string {
	t.Helper()
	got := iw.allWritesTo(ref)
	if len(got) != want {
		t.Fatalf("%s has %d writes, want %d: %+v", ref, len(got), want, got)
	}
	if want == 0 {
		return ""
	}
	c := got[want-1]
	if c.Message.Kind != msg.Inform || c.Delivery.Inbox != "coord" || !c.Delivery.VerifiedSender {
		t.Fatalf("notice %+v inbox %q verified %v", c.Message, c.Delivery.Inbox, c.Delivery.VerifiedSender)
	}
	return c.Message.Text()
}

func TestInboxMembershipNotices(t *testing.T) {
	w := newWorld(t)
	w.codex.addThread("T-bob", "bob")
	w.codex.addThread("T-carol", "carol")
	iw := &inboxWorld{w: w, bob: identity.AgentID("codex", "T-bob"), carol: identity.AgentID("codex", "T-carol")}
	iw.nb = w.node(1, w.lb, nil)
	iw.nb.ok(thread("T-bob"), "receive_message", map[string]any{})
	iw.nb.ok(thread("T-carol"), "receive_message", map[string]any{})
	B, C := thread("T-bob"), thread("T-carol")

	iw.nb.ok(B, "attach", inboxArg("coord"))
	iw.lastWriteTo(t, "T-bob", 0)

	iw.nb.ok(B, "grant", map[string]any{"inbox": "coord", "to": iw.carol})
	iw.lastWriteTo(t, "T-bob", 0)
	iw.nb.ok(C, "attach", inboxArg("coord"))
	text := iw.lastWriteTo(t, "T-bob", 1)
	if !strings.Contains(text, "carol") || !strings.Contains(text, "attached") || !strings.Contains(text, "Responder now: carol") {
		t.Fatalf("attach notice %q", text)
	}
	iw.lastWriteTo(t, "T-carol", 0)
	iw.nb.ok(C, "attach", inboxArg("coord"))
	iw.lastWriteTo(t, "T-bob", 1)

	iw.nb.ok(B, "grant", map[string]any{"inbox": "coord", "to": iw.carol, "owner": true})
	text = iw.lastWriteTo(t, "T-carol", 1)
	if !strings.Contains(text, "bob") || !strings.Contains(text, "handed ownership") || !strings.Contains(text, "Owner: carol") {
		t.Fatalf("hand-over notice %q", text)
	}

	iw.nb.ok(C, "detach", inboxArg("coord"))
	text = iw.lastWriteTo(t, "T-bob", 2)
	if !strings.Contains(text, "carol") || !strings.Contains(text, "detached") || !strings.Contains(text, "Responder now: bob") {
		t.Fatalf("detach notice %q", text)
	}

	iw.nb.ok(C, "claim", map[string]any{"name": "inbox:coord"})
	text = iw.lastWriteTo(t, "T-bob", 3)
	if !strings.Contains(text, "carol") || !strings.Contains(text, "claimed") || !strings.Contains(text, "Owner: carol") {
		t.Fatalf("claim notice %q", text)
	}

	if _, out := iw.nb.call(B, "reply", map[string]any{"message_id": iw.w.codex.Writes()[0].Message.MessageID, "text": "ok?"}); out["reason"] != ReasonInboxCopy {
		t.Fatalf("a notice opened a reply route: %v", out)
	}
}

func TestInboxHandover(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	pred := w.claude(4002, "sess-pred", "role-1")
	w.codex.addThread("T-succ", "role-2")
	na := w.node(alice.pid, w.lb, nil)
	np := w.node(pred.pid, w.lb, nil)
	ns := w.node(1, w.lb, nil)
	S := thread("T-succ")
	succ := identity.AgentID("codex", "T-succ")
	na.ok(nil, "receive_message", map[string]any{})
	reaches := func(step string, responder string) map[string]any {
		t.Helper()
		res := na.ok(nil, "send_message", send("inbox:role", step))
		if res["state"] != "accepted" || res["responder"] != responder {
			t.Fatalf("%s: %v, want responder %s", step, res, responder)
		}
		return res
	}

	np.ok(nil, "attach", inboxArg("role"))
	reaches("step 1", pred.agentID())
	held := na.ok(nil, "ask", map[string]any{"to": "inbox:role", "text": "held by P?"})["message_id"].(string)

	ns.ok(S, "receive_message", map[string]any{})
	np.ok(nil, "grant", map[string]any{"inbox": "role", "to": succ})
	reaches("step 2", pred.agentID())

	if out := ns.ok(S, "attach", inboxArg("role")); out["created"] != false || out["owner"] != pred.agentID() {
		t.Fatalf("successor attach %v", out)
	}
	reaches("step 3", succ)
	if !slicesContain(pred.got(), "role-2", "attached") {
		t.Fatalf("predecessor got no attach notice: %q", pred.got())
	}

	np.ok(nil, "grant", map[string]any{"inbox": "role", "to": succ, "owner": true})
	if out := ns.ok(S, "attach", inboxArg("role")); out["owner"] != succ {
		t.Fatalf("successor does not own the inbox: %v", out)
	}
	reaches("step 4", succ)

	before := len(alice.got())
	if rep := np.ok(nil, "reply", map[string]any{"message_id": held, "text": "main"}); rep["state"] != "accepted" {
		t.Fatalf("predecessor's reply %v", rep)
	}
	if got := alice.got(); len(got) != before+1 || !strings.Contains(got[len(got)-1], "main") {
		t.Fatalf("alice got %q", got)
	}

	np.ok(nil, "detach", inboxArg("role"))
	var succGot []string
	for _, c := range w.codex.Writes() {
		succGot = append(succGot, c.Message.Text())
	}
	if !slicesContain(succGot, "role-1", "detached") {
		t.Fatalf("successor got no detach notice: %q", succGot)
	}
	if d, _ := w.descriptor(succ); len(d.Inboxes) != 1 {
		t.Fatalf("successor descriptor %+v", d.Inboxes)
	}
	reaches("step 6", succ)
	if out := na.ok(nil, "send_message", send("role-1", "old name")); out["state"] != "accepted" {
		t.Fatalf("old name before exit %v", out)
	}

	np.stop()
	if err := os.Remove(filepath.Join(w.sessions, "4002.json")); err != nil {
		t.Fatal(err)
	}
	reaches("step 7", succ)
	if out := na.ok(nil, "send_message", send("role-1", "old name")); out["reason"] != ReasonNoSuchAgent {
		t.Fatalf("old generation-numbered name after exit %v", out)
	}
}

func TestInboxColdHandoverClaim(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	pred := w.claude(4002, "sess-pred", "role-1")
	w.codex.addThread("T-succ", "role-2")
	na := w.node(alice.pid, w.lb, nil)
	np := w.node(pred.pid, w.lb, nil)
	ns := w.node(1, w.lb, nil)
	S := thread("T-succ")
	succ := identity.AgentID("codex", "T-succ")
	na.ok(nil, "receive_message", map[string]any{})
	ns.ok(S, "receive_message", map[string]any{})

	np.ok(nil, "attach", inboxArg("role"))
	np.stop()
	if err := os.Remove(filepath.Join(w.sessions, "4002.json")); err != nil {
		t.Fatal(err)
	}
	if out := na.ok(nil, "send_message", send("inbox:role", "anyone?")); out["reason"] != ReasonNoSuchAgent {
		t.Fatalf("between the sessions %v", out)
	}
	ns.fails(S, "attach", inboxArg("role"), "rejected", "ownerless")
	if out := ns.ok(S, "claim", map[string]any{"name": "inbox:role"}); out["owner"] != succ {
		t.Fatalf("claim %v", out)
	}
	if out := na.ok(nil, "send_message", send("inbox:role", "now?")); out["state"] != "accepted" || out["responder"] != succ {
		t.Fatalf("after the claim %v", out)
	}
}

func inboxArg(n string) map[string]any { return map[string]any{"inbox": n} }

func slicesContain(texts []string, subs ...string) bool {
	for _, t := range texts {
		ok := true
		for _, s := range subs {
			ok = ok && strings.Contains(t, s)
		}
		if ok {
			return true
		}
	}
	return false
}
