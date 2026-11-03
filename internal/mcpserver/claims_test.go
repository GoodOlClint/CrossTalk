package mcpserver

import (
	"strings"
	"testing"

	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

func claimEnv(t *testing.T) *env {
	fb := NewFakeBackend()
	fb.SetSessions(
		Session{Harness: "codex", SessionRef: "A", Name: "a", Cwd: "/w"},
		Session{Harness: "codex", SessionRef: "B", Name: "b", Cwd: "/w"},
		Session{Harness: "codex", SessionRef: "C", Name: "c", Cwd: "/w"},
	)
	e := newEnv(t, 999999, fb)
	for _, th := range []string{"A", "B", "C"} {
		e.mustOK(thread(th), "list_agents", map[string]any{})
	}
	return e
}

func claimRecord(t *testing.T, e *env, agentID string) journal.Record {
	t.Helper()
	a := e.srv.local(agentID)
	if a == nil {
		t.Fatalf("%s not served", agentID)
	}
	r, _ := a.Journal().Latest(journal.Record{Type: journal.TypeClaim})
	return r
}

func lastSend(t *testing.T, e *env) FakeCall {
	t.Helper()
	cs := e.fb.Calls()
	for i := len(cs) - 1; i >= 0; i-- {
		if cs[i].Op == "send" {
			return cs[i]
		}
	}
	t.Fatal("no send")
	return FakeCall{}
}

// Test 28 (in-process part).
func TestClaimRelease(t *testing.T) {
	e := claimEnv(t)
	A, B, C := thread("A"), thread("B"), thread("C")

	e.mustErr(A, "claim", map[string]any{"name": "a"}, CodeRejected, "self")
	e.mustErr(A, "claim", map[string]any{"name": "nobody"}, CodeRejected, "no_such_agent")
	e.mustErr(A, "release", map[string]any{"name": "b"}, CodeRejected, "not_claimed")

	out := e.mustOK(A, "claim", map[string]any{"name": "b"})
	if out["name"] != "b" || out["owner"] != "a" {
		t.Fatalf("claim result %v", out)
	}
	if inf, _ := out["inform"].(map[string]any); inf["state"] != "accepted" {
		t.Errorf("claim inform %v", out)
	}
	if s := lastSend(t, e); s.From != codexID("A") || s.Send.To != "b" || s.Send.Kind != "inform" || !strings.Contains(s.Send.Text, "claimed") {
		t.Errorf("target not informed: %+v", s)
	}
	if r := claimRecord(t, e, codexID("B")); !r.Claimed || r.Owner != codexID("A") || r.Binding.SessionRef != "B" {
		t.Errorf("claim not journaled in target's journal by agent_id: %+v", r)
	}
	if r := claimRecord(t, e, codexID("A")); r.Type != "" {
		t.Errorf("claim journaled in owner's journal: %+v", r)
	}
	d := e.descriptor(codexID("B"))
	if d.Role != registry.RoleSubagent || d.Owner == nil || *d.Owner != "a" {
		t.Errorf("target descriptor %+v", d)
	}
	rows := rowsByID(t, e.mustOK(nil, "list_agents", map[string]any{}))
	if r := rows[codexID("B")]; r["role"] != "subagent" || r["owner"] != "a" {
		t.Errorf("list row %v", r)
	}

	e.mustErr(C, "claim", map[string]any{"name": "b"}, CodeRejected, "already_claimed")
	e.mustErr(C, "release", map[string]any{"name": "b"}, CodeRejected, "not_owner")
	e.mustErr(B, "release", map[string]any{"name": "b"}, CodeRejected, "self")
	e.mustErr(B, "claim", map[string]any{"name": "b"}, CodeRejected, "self")
	e.mustErr(B, "claim", map[string]any{"name": "c"}, CodeRejected, "caller_not_primary")

	out = e.mustOK(A, "release", map[string]any{"name": "b"})
	if out["name"] != "b" {
		t.Fatalf("release result %v", out)
	}
	if s := lastSend(t, e); s.Send.To != "b" || !strings.Contains(s.Send.Text, "released") {
		t.Errorf("release inform %+v", s)
	}
	if d := e.descriptor(codexID("B")); d.Role != registry.RolePrimary || d.Owner != nil {
		t.Errorf("after release %+v", d)
	}
	if r := claimRecord(t, e, codexID("B")); r.Claimed {
		t.Errorf("release not journaled: %+v", r)
	}
	e.mustOK(C, "claim", map[string]any{"name": "b"})
}

func TestClaimSurvivesReRegistration(t *testing.T) {
	e := claimEnv(t)
	e.mustOK(thread("A"), "claim", map[string]any{"name": "b"})
	e.stop()
	e.start(999999)
	e.mustOK(thread("A"), "list_agents", map[string]any{})
	e.mustOK(thread("B"), "list_agents", map[string]any{})
	if d := e.descriptor(codexID("B")); d.Role != registry.RoleSubagent || d.Owner == nil || *d.Owner != "a" {
		t.Fatalf("claim not replayed from journal: %+v", d)
	}
	e.mustErr(thread("C"), "release", map[string]any{"name": "b"}, CodeRejected, "not_owner")
	e.mustOK(thread("A"), "release", map[string]any{"name": "b"})
}

func TestClaimRemoteTargetGoesThroughBackend(t *testing.T) {
	e := claimEnv(t)
	other := registry.New(e.stateDir)
	g, err := other.Acquire(registry.Request{AgentID: "dddddddddddddddd", HarnessName: "d", Harness: "claude", Cwd: "/w"})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if err := g.Publish(registry.Descriptor{SessionRef: "D"}); err != nil {
		t.Fatal(err)
	}
	e.mustOK(thread("A"), "claim", map[string]any{"name": "d"})
	var claimed bool
	for _, c := range e.fb.Calls() {
		if c.Op == "claim" && c.Target == "dddddddddddddddd" && c.From == codexID("A") {
			claimed = true
		}
	}
	if !claimed {
		t.Fatalf("remote claim not delegated: %+v", e.fb.Calls())
	}
	e.mustErr(thread("A"), "release", map[string]any{"name": "d"}, CodeRejected, "not_claimed")

	owner := "a"
	if err := g.Publish(registry.Descriptor{SessionRef: "D", Role: registry.RoleSubagent, Owner: &owner}); err != nil {
		t.Fatal(err)
	}
	e.mustErr(thread("C"), "claim", map[string]any{"name": "d"}, CodeRejected, "already_claimed")
	e.fb.ClaimErr = rejected("not_owner")
	e.mustErr(thread("C"), "release", map[string]any{"name": "d"}, CodeRejected, "not_owner")
}
