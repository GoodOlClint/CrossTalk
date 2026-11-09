package shim

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
)

const backendMarker = "OpenAI backend"

func backendWorld(t *testing.T) (*world, *node) {
	t.Helper()
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	w.codex.addThread("T-bob", "bob")
	w.codex.SetBinding(adapter.Binding{Harness: adapter.Codex, SessionRef: "T-bob", HarnessEndpoint: "fake://T-bob"})
	na := w.node(alice.pid, w.lb, nil)
	nb := w.node(1, w.lb, nil)
	nb.ok(thread("T-bob"), "receive_message", map[string]any{})
	return w, na
}

func hintOf(res map[string]any) string {
	h, _ := res["hint"].(string)
	return h
}

func TestBackendHintOnAcceptedAndQueued(t *testing.T) {
	for _, state := range []adapter.State{adapter.Accepted, adapter.Queued} {
		w, na := backendWorld(t)
		w.codex.setBackend("unreachable")
		w.codex.ScriptInject(adapter.Outcome{State: state})
		res := na.ok(nil, "send_message", send("bob", "hi"))
		if res["state"] != string(state) {
			t.Fatalf("%s: send %v", state, res)
		}
		h := hintOf(res)
		if !strings.Contains(h, hintCodexBackendDown) || !strings.Contains(h, "is in the thread") || !strings.Contains(h, "daemon restart") {
			t.Fatalf("%s: hint %q", state, h)
		}
		if !slices.Contains(w.codex.backendEps, "fake://T-bob") {
			t.Fatalf("Backend asked about %v", w.codex.backendEps)
		}
	}
}

func TestBackendHintOnUnknownMakesNoDeliveryClaim(t *testing.T) {
	w, na := backendWorld(t)
	w.codex.setBackend("unreachable")
	w.codex.ScriptInject(adapter.Outcome{State: adapter.Unknown, Err: errors.New("no response")})
	res := na.ok(nil, "send_message", send("bob", "hi"))
	if res["state"] != "unknown" {
		t.Fatalf("send %v", res)
	}
	h := hintOf(res)
	if !strings.Contains(h, hintCodexBackendDownUnknown) || strings.Contains(h, "is in the thread") || !strings.Contains(h, "receive_message") {
		t.Fatalf("hint %q", h)
	}
}

func TestNoBackendHintWhenReachableUnknownOrNotDelivered(t *testing.T) {
	for _, c := range []struct {
		backend string
		out     adapter.Outcome
	}{
		{"reachable", adapter.Outcome{State: adapter.Accepted}},
		{"unknown", adapter.Outcome{State: adapter.Accepted}},
		{"", adapter.Outcome{State: adapter.Accepted}},
		{"unreachable", adapter.Outcome{State: adapter.Unreachable, Err: errors.New("refused")}},
	} {
		w, na := backendWorld(t)
		w.codex.setBackend(c.backend)
		w.codex.ScriptInject(c.out)
		res := na.ok(nil, "send_message", send("bob", "hi"))
		if h := hintOf(res); strings.Contains(h, backendMarker) {
			t.Errorf("backend %q, state %v: hint %q", c.backend, res["state"], h)
		}
	}
}

func TestBackendHintJoinsNudge(t *testing.T) {
	w, na := backendWorld(t)
	w.codex.setBackend("unreachable")
	var last map[string]any
	for range NudgeFirst {
		last = na.ok(nil, "send_message", send("bob", "x"))
	}
	h := hintOf(last)
	if !strings.Contains(h, hintCodexBackendDown) || !nudged(last) {
		t.Fatalf("hint %q", h)
	}
}

func TestBackendHintOnNativeInject(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	w.codex.addThread("T-eve", "eve")
	w.codex.SetBinding(adapter.Binding{Harness: adapter.Codex, SessionRef: "T-eve", HarnessEndpoint: "fake://T-eve"})
	w.codex.setBackend("unreachable")
	na := w.node(alice.pid, w.lb, nil)
	res := na.ok(nil, "send_message", send("eve", "hi"))
	h := hintOf(res)
	if res["state"] != "accepted" || !strings.Contains(h, hintCodexBackendDown) || !strings.Contains(h, hintNativeOneWay) {
		t.Fatalf("send %v", res)
	}
}

func TestNoBackendHintForClaudeTarget(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	w.claude(4002, "sess-bob", "bob")
	w.codex.setBackend("unreachable")
	na := w.node(alice.pid, nil, nil)
	if h := hintOf(na.ok(nil, "send_message", send("bob", "hi"))); strings.Contains(h, backendMarker) {
		t.Fatalf("hint %q", h)
	}
}

func TestNoBackendHintForCustomProviderThread(t *testing.T) {
	w, na := backendWorld(t)
	w.codex.setBackend("unreachable")
	w.codex.setCustomProvider("T-bob")
	if h := hintOf(na.ok(nil, "send_message", send("bob", "hi"))); strings.Contains(h, backendMarker) {
		t.Errorf("registered: hint %q", h)
	}

	w = newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	w.codex.addThread("T-eve", "eve")
	w.codex.SetBinding(adapter.Binding{Harness: adapter.Codex, SessionRef: "T-eve", HarnessEndpoint: "fake://T-eve"})
	w.codex.setBackend("unreachable")
	w.codex.setCustomProvider("T-eve")
	na = w.node(alice.pid, w.lb, nil)
	res := na.ok(nil, "send_message", send("eve", "hi"))
	if h := hintOf(res); res["state"] != "accepted" || strings.Contains(h, backendMarker) {
		t.Errorf("native: send %v", res)
	}
}
