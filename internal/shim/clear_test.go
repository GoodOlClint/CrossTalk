package shim

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoodOlClint/crosstalk/internal/identity"
	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

func rows(t *testing.T, out map[string]any, name string) []map[string]any {
	t.Helper()
	var got []map[string]any
	for _, r := range out["agents"].([]any) {
		if m := r.(map[string]any); m["name"] == name {
			got = append(got, m)
		}
	}
	return got
}

func hasAgent(out map[string]any, agentID string) bool {
	for _, r := range out["agents"].([]any) {
		if r.(map[string]any)["agent_id"] == agentID {
			return true
		}
	}
	return false
}

func recv(n *node, meta map[string]any) map[string]any {
	n.t.Helper()
	return n.ok(meta, "receive_message", map[string]any{})
}

func TestClearKeepsNameAndDeliveryByName(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	bob := w.claude(4002, "sess-1", "bob")
	na := w.node(alice.pid, w.lb, nil)
	nb := w.node(bob.pid, w.lb, nil)
	recv(na, nil)
	recv(nb, nil)
	oldID := bob.agentID()
	if out := na.ok(nil, "send_message", send("bob", "before")); out["state"] != "accepted" {
		t.Fatalf("before clear %v", out)
	}

	w.become(bob, "sess-2", "bob")
	out := nb.ok(nil, "list_agents", map[string]any{})
	if out["notice"] != nil {
		t.Errorf("false notice after /clear: %v", out["notice"])
	}
	got := rows(t, out, "bob")
	if len(got) != 1 || got[0]["agent_id"] != bob.agentID() || got[0]["crosstalk"] != true || got[0]["reachable"] != true {
		t.Fatalf("bob rows %v", got)
	}
	if hasAgent(out, oldID) {
		t.Errorf("old session still listed: %v", out["agents"])
	}
	if d, ok := w.descriptor(bob.agentID()); !ok || d.Name != "bob" {
		t.Fatalf("new descriptor %+v %v", d, ok)
	}
	if _, err := os.Stat(journal.Path(w.state, oldID)); err != nil {
		t.Errorf("old journal: %v", err)
	}

	res := na.ok(nil, "send_message", send("bob", "after"))
	if res["state"] != "accepted" {
		t.Fatalf("delivery by name after /clear %v", res)
	}
	waitFor(t, "delivery to the new session", func() bool {
		g := bob.got()
		return len(g) == 2 && strings.Contains(g[1], "after")
	})
}

func TestClearBeforeAnyCallRoutesNativeNotToDeadRecord(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	bob := w.claude(4002, "sess-1", "bob")
	na := w.node(alice.pid, w.lb, nil)
	nb := w.node(bob.pid, w.lb, nil)
	recv(na, nil)
	recv(nb, nil)
	oldID := bob.agentID()

	w.become(bob, "sess-2", "bob")
	out := na.ok(nil, "list_agents", map[string]any{})
	got := rows(t, out, "bob")
	if len(got) != 1 || got[0]["agent_id"] != bob.agentID() || got[0]["crosstalk"] != false || got[0]["reachable"] != true {
		t.Fatalf("bob rows %v", got)
	}
	if hasAgent(out, oldID) {
		t.Errorf("old session listed: %v", out["agents"])
	}
	if res := na.ok(nil, "send_message", send("bob", "hi")); res["state"] != "accepted" {
		t.Fatalf("by name %v", res)
	}
	if res := na.ok(nil, "ask", send("bob", "q?")); res["reason"] != ReasonNativeInformOnly || !strings.Contains(fmt.Sprint(res["hint"]), "send_message") {
		t.Fatalf("ask %v", res)
	}
	if res := na.ok(nil, "send_message", send(oldID, "x")); res["reason"] != ReasonNoSuchAgent {
		t.Fatalf("old id %v", res)
	}
}

func TestLiveHolderForcesSuffixAndNameReturnsOnRelease(t *testing.T) {
	w := newWorld(t)
	bob := w.claude(4002, "sess-1", "bob")
	carol := w.claude(4003, "sess-c", "carol")
	w.become(carol, "sess-c", "bob")
	nb := w.node(bob.pid, nil, nil)
	nc := w.node(carol.pid, nil, nil)
	recv(nb, nil)
	out := recv(nc, nil)
	n, _ := out["notice"].(string)
	if !strings.Contains(n, "another live agent") {
		t.Fatalf("notice %q", n)
	}
	d, ok := w.descriptor(carol.agentID())
	if !ok || d.Name == "bob" || !strings.Contains(d.Name, carol.agentID()[:8]) {
		t.Fatalf("carol %+v", d)
	}
	if d, _ := w.descriptor(bob.agentID()); d.Name != "bob" {
		t.Fatalf("live holder lost its name: %+v", d)
	}
	if out := recv(nc, nil); out["notice"] != nil {
		t.Errorf("notice repeats: %v", out["notice"])
	}

	nb.stop()
	out = recv(nc, nil)
	if d, _ := w.descriptor(carol.agentID()); d.Name != "bob" {
		t.Fatalf("freed name not taken: %+v", d)
	}
	if n, _ := out["notice"].(string); !strings.Contains(n, "now registered") {
		t.Errorf("notice %q", n)
	}
}

func TestPooledCodexThreadsDoNotEvictEachOther(t *testing.T) {
	w := newWorld(t)
	w.codex.addThread("T-a", "ax")
	w.codex.addThread("T-b", "bx")
	w.codex.addThread("T-c", "cx")
	n := w.node(999999, nil, nil)
	recv(n, thread("T-a"))
	recv(n, thread("T-b"))
	recv(n, thread("T-c"))
	st := func(id string) *AgentState { return n.sh.Agent(identity.AgentID("codex", id)) }
	for _, id := range []string{"T-a", "T-b", "T-c"} {
		if st(id) == nil {
			t.Errorf("%s evicted by a sibling thread", id)
		}
	}
	out := n.ok(thread("T-a"), "list_agents", map[string]any{})
	for _, name := range []string{"ax", "bx", "cx"} {
		if g := rows(t, out, name); len(g) != 1 || g[0]["crosstalk"] != true {
			t.Errorf("%s rows %v", name, g)
		}
	}
}

func TestResumeOfOldSessionTakesNameAndAnswersOldAsk(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	bob := w.claude(4002, "sess-1", "bob")
	na := w.node(alice.pid, w.lb, nil)
	nb := w.node(bob.pid, w.lb, nil)
	recv(na, nil)
	recv(nb, nil)
	oldID := bob.agentID()
	ask := na.ok(nil, "ask", send("bob", "question?"))
	if ask["state"] != "accepted" {
		t.Fatalf("ask %v", ask)
	}

	w.become(bob, "sess-2", "bob")
	recv(nb, nil)
	clearedID := bob.agentID()
	if d, _ := w.descriptor(clearedID); d.Name != "bob" {
		t.Fatalf("after clear %+v", d)
	}

	w.become(bob, "sess-1", "bob")
	out := nb.ok(nil, "list_agents", map[string]any{})
	if out["notice"] != nil {
		t.Errorf("notice on resume: %v", out["notice"])
	}
	if g := rows(t, out, "bob"); len(g) != 1 || g[0]["agent_id"] != oldID {
		t.Fatalf("bob rows %v", g)
	}
	if hasAgent(out, clearedID) {
		t.Errorf("cleared session still listed")
	}
	reply := nb.ok(nil, "reply", map[string]any{"message_id": ask["message_id"], "text": "answer"})
	if reply["state"] != "accepted" {
		t.Fatalf("reply after resume %v", reply)
	}
	waitFor(t, "the reply at the asker", func() bool {
		g := alice.got()
		return len(g) == 1 && strings.Contains(g[0], "answer")
	})
}

func TestResumeElsewhereWhileNameHeldLiveGetsSuffixAndKeepsJournal(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	bob := w.claude(4002, "sess-1", "bob")
	carol := w.claude(4003, "sess-c", "carol")
	na := w.node(alice.pid, w.lb, nil)
	nb := w.node(bob.pid, w.lb, nil)
	nc := w.node(carol.pid, w.lb, nil)
	recv(na, nil)
	recv(nb, nil)
	oldID := bob.agentID()
	ask := na.ok(nil, "ask", send("bob", "question?"))

	w.become(bob, "sess-2", "bob")
	recv(nb, nil)
	w.become(carol, "sess-1", "bob")
	out := recv(nc, nil)
	n, _ := out["notice"].(string)
	if !strings.Contains(n, "another live agent") {
		t.Fatalf("notice %q", n)
	}
	d, ok := w.descriptor(oldID)
	if !ok || d.Name == "bob" {
		t.Fatalf("resumed %+v %v", d, ok)
	}
	if d, _ := w.descriptor(bob.agentID()); d.Name != "bob" {
		t.Fatalf("live holder %+v", d)
	}
	if reply := nc.ok(nil, "reply", map[string]any{"message_id": ask["message_id"], "text": "answer"}); reply["state"] != "accepted" {
		t.Fatalf("reply %v", reply)
	}
}

func TestStaleDescriptorFromDeadShimHoldsNothing(t *testing.T) {
	w := newWorld(t)
	bob := w.claude(4002, "sess-2", "bob")
	stale := registry.Descriptor{AgentID: "00000000deadbeef", Name: "bob", Harness: "claude", SessionRef: "sess-old", PID: 999999, InstanceID: "old", Protocol: 1}
	b, _ := json.Marshal(stale)
	reg := registry.New(w.state)
	if err := os.MkdirAll(filepath.Dir(reg.DescriptorPath(stale.AgentID)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reg.DescriptorPath(stale.AgentID), b, 0o600); err != nil {
		t.Fatal(err)
	}
	nb := w.node(bob.pid, nil, nil)
	out := nb.ok(nil, "list_agents", map[string]any{})
	if out["notice"] != nil {
		t.Errorf("notice %v", out["notice"])
	}
	if g := rows(t, out, "bob"); len(g) != 1 || g[0]["agent_id"] != bob.agentID() {
		t.Fatalf("rows %v", g)
	}
	if _, err := os.Stat(reg.DescriptorPath(stale.AgentID)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stale descriptor not pruned: %v", err)
	}
}
