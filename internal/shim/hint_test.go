package shim

import (
	"os"
	"strings"
	"testing"
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
	if res := na.ok(nil, "ask", send("ghost", "q?")); res["hint"] != nil {
		t.Fatalf("no such agent carries a hint: %v", res)
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
