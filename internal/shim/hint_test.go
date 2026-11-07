package shim

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/harness/codex"
)

func TestNativeInformOnlyCarriesHonestHint(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	w.claude(4002, "sess-bob", "bob")
	if err := os.WriteFile(w.sessions+"/4003.json", []byte(`{"pid":4003,"sessionId":"sess-quiet","name":"quiet","cwd":"/q","status":"idle","messagingSocketPath":"/nonexistent.sock"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	na := w.node(alice.pid, nil, nil)

	res := na.ok(nil, "ask", send("bob", "q?"))
	if res["state"] != "rejected" || res["reason"] != ReasonNativeInformOnly {
		t.Fatalf("ask %v", res)
	}
	if h, _ := res["hint"].(string); !strings.Contains(h, "send_message") || !strings.Contains(h, "binds it") {
		t.Fatalf("hint %q", h)
	}
	cmd := na.ok(nil, "send_message", map[string]any{"to": "bob", "text": "x", "kind": "command"})
	if cmd["reason"] != ReasonNativeInformOnly || cmd["hint"] != res["hint"] {
		t.Fatalf("command %v", cmd)
	}
	if h, _ := na.ok(nil, "ask", send("quiet", "q?"))["hint"].(string); !strings.Contains(h, "unreachable") || strings.Contains(h, "send_message") {
		t.Fatalf("unreachable native hint %q", h)
	}
}

func TestAmbiguousNameIsNotNativeInformOnly(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	d1 := w.claude(4002, "sess-d1", "d1")
	d2 := w.claude(4003, "sess-d2", "d2")
	w.become(d1, "sess-d1", "dup")
	w.become(d2, "sess-d2", "dup")
	na := w.node(alice.pid, nil, nil)

	res := na.ok(nil, "ask", send("dup", "q?"))
	if res["state"] != "rejected" || res["reason"] != ReasonAmbiguousName {
		t.Fatalf("ask %v", res)
	}
	h, _ := res["hint"].(string)
	if !strings.Contains(h, "agent_id") || strings.Contains(h, "binding") {
		t.Fatalf("hint %q", h)
	}
	if res := na.ok(nil, "send_message", send("dup", "x")); res["reason"] != ReasonAmbiguousName || res["hint"] != h {
		t.Fatalf("inform %v", res)
	}
	if res := na.ok(nil, "ask", send(d1.agentID(), "q?")); res["reason"] != ReasonNativeInformOnly {
		t.Fatalf("by id %v", res)
	}
}

func TestReasonsCarryHints(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	na := w.node(alice.pid, nil, nil)
	if h, _ := na.ok(nil, "ask", send("ghost", "q?"))["hint"].(string); !strings.Contains(h, "list_agents") {
		t.Fatalf("no_such_agent hint %q", h)
	}
	if h, _ := na.ok(nil, "send_message", send("alice", "me"))["hint"].(string); !strings.Contains(h, "you: true") {
		t.Fatalf("self hint %q", h)
	}
	_, out := na.call(nil, "reply", map[string]any{"message_id": "nope", "text": "x"})
	if h, _ := out["hint"].(string); out["reason"] != ReasonNoSuchMessage || !strings.Contains(h, "send_message") {
		t.Fatalf("no_such_message %v", out)
	}
}

func TestAgentHeldHintSaysWhatToRemove(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	n1 := w.node(alice.pid, nil, nil)
	n2 := w.node(alice.pid, nil, nil)
	n1.ok(nil, "receive_message", map[string]any{})
	_, out := n2.call(nil, "receive_message", map[string]any{})
	if h, _ := out["hint"].(string); out["reason"] != "agent_held" || !strings.Contains(h, "claude mcp remove") {
		t.Fatalf("agent_held %v", out)
	}
}

func TestListAgentsNoticeCarriesAgentHeldHint(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	n1 := w.node(alice.pid, nil, nil)
	n2 := w.node(alice.pid, nil, nil)
	n1.ok(nil, "receive_message", map[string]any{})
	notice, _ := n2.ok(nil, "list_agents", map[string]any{})["notice"].(string)
	if !strings.Contains(notice, "agent_held") || !strings.Contains(notice, "claude mcp remove") {
		t.Fatalf("notice %q", notice)
	}
}

func TestReplyToFirstContactInformHintsSendMessage(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	w.codex.addThread("T-bob", "bob")
	na := w.node(alice.pid, w.lb, nil)
	id := na.ok(nil, "send_message", send("bob", "hello"))["message_id"]
	nb := w.node(1, w.lb, nil)
	nb.ok(thread("T-bob"), "receive_message", map[string]any{})
	_, out := nb.call(thread("T-bob"), "reply", map[string]any{"message_id": id, "text": "hi"})
	if h, _ := out["hint"].(string); out["reason"] != ReasonNoSuchMessage || !strings.Contains(h, "send_message") {
		t.Fatalf("reply %v", out)
	}
}

func TestNoSuchMessageHintsAreTrueOnEveryPath(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	w.codex.addThread("T-bob", "bob")
	t0 := time.Date(2026, 11, 3, 13, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	now := t0
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	na := w.node(alice.pid, w.lb, nil)
	nb := w.node(1, w.lb, func(o *Options) { o.Now = clock })
	nb.ok(thread("T-bob"), "receive_message", map[string]any{})
	hint := func(id any) string {
		_, out := nb.call(thread("T-bob"), "reply", map[string]any{"message_id": id, "text": "x"})
		if out["reason"] != ReasonNoSuchMessage {
			t.Fatalf("reply %v: %v", id, out)
		}
		h, _ := out["hint"].(string)
		return h
	}

	if h := hint("mistyped-id"); strings.Contains(h, "nothing expired") || strings.Contains(h, "arrived before this session bound, so") || !strings.Contains(h, "check the message_id") || !strings.Contains(h, "send_message") {
		t.Fatalf("unknown id hint %q", h)
	}

	w.codex.ScriptInject(adapter.Outcome{State: adapter.Unreachable, Err: errors.New("down")})
	failed := na.ok(nil, "send_message", send("bob", "never injected"))["message_id"]
	if h := hint(failed); strings.Contains(h, "one hour") || !strings.Contains(h, "no open reply route") || !strings.Contains(h, "send_message") {
		t.Fatalf("closed route hint %q", h)
	}

	old := na.ok(nil, "send_message", send("bob", "fyi"))["message_id"]
	mu.Lock()
	now = now.Add(61 * time.Minute)
	mu.Unlock()
	if h := hint(old); !strings.Contains(h, "no open reply route") || !strings.Contains(h, "send_message") {
		t.Fatalf("expired hint %q", h)
	}
}

func TestAcceptedNativeSendWarnsOneWay(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	w.claude(4002, "sess-bob", "bob")
	na := w.node(alice.pid, nil, nil)
	res := na.ok(nil, "send_message", send("bob", "fyi"))
	if res["state"] != "accepted" {
		t.Fatalf("send %v", res)
	}
	if h, _ := res["hint"].(string); !strings.Contains(h, "one way") || !strings.Contains(h, "wait on that session's user") {
		t.Fatalf("hint %q", h)
	}
}

func TestPulledMessageCarriesHarnessNotice(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	w.codex.addThread("T-bob", "bob")
	na := w.node(alice.pid, w.lb, nil)
	nb := w.node(1, w.lb, nil)
	na.ok(nil, "receive_message", map[string]any{})
	nb.ok(thread("T-bob"), "receive_message", map[string]any{})
	w.codex.ScriptInject(adapter.Outcome{State: adapter.Unreachable, Err: errors.New("down")})
	na.ok(nil, "send_message", send("bob", "please read README.md"))

	es := nb.ok(thread("T-bob"), "receive_message", map[string]any{})["messages"].([]any)
	if len(es) != 1 {
		t.Fatalf("mailbox %v", es)
	}
	e := es[0].(map[string]any)
	if e["notice"] != codex.Preamble || !strings.Contains(e["text"].(string), "README") {
		t.Fatalf("entry %v", e)
	}
}

func uuidV7At(ts time.Time) string {
	ms := uint64(ts.UnixMilli())
	return fmt.Sprintf("%08x-%04x-7abc-8def-0123456789ab", ms>>16, ms&0xffff)
}

func TestReplyHintSeparatesPreBindFromUnknownID(t *testing.T) {
	w := newWorld(t)
	w.codex.addThread("T-bob", "bob")
	t0 := time.Date(2026, 11, 3, 13, 0, 0, 0, time.UTC)
	nb := w.node(1, w.lb, func(o *Options) { o.Now = func() time.Time { return t0.Add(500 * time.Microsecond) } })
	nb.ok(thread("T-bob"), "receive_message", map[string]any{})
	hint := func(id string) string {
		_, out := nb.call(thread("T-bob"), "reply", map[string]any{"message_id": id, "text": "x"})
		if out["reason"] != ReasonNoSuchMessage {
			t.Fatalf("reply %v: %v", id, out)
		}
		h, _ := out["hint"].(string)
		return h
	}

	pre := hint(uuidV7At(t0.Add(-time.Hour)))
	if !strings.Contains(pre, "may predate this session's binding") || !strings.Contains(pre, "check the message_id") || !strings.Contains(pre, "send_message") {
		t.Fatalf("pre-bind hint %q", pre)
	}
	for name, id := range map[string]string{
		"newer":     uuidV7At(t0.Add(time.Hour)),
		"same-ms":   uuidV7At(t0),
		"malformed": "nope",
		"v4":        "3f2b8c1e-5d4a-4b6f-9a7e-0123456789ab",
	} {
		h := hint(id)
		if strings.Contains(h, "predates") || !strings.Contains(h, "check the message_id") || !strings.Contains(h, "send_message") {
			t.Fatalf("%s hint %q", name, h)
		}
	}
}
