package mcpserver

import (
	"strings"
	"testing"

	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

func renameEnv(t *testing.T, names map[string]string) *env {
	t.Helper()
	fb := NewFakeBackend()
	var ss []Session
	for ref, name := range names {
		ss = append(ss, Session{Harness: "codex", SessionRef: ref, Name: name, Cwd: "/w"})
	}
	fb.SetSessions(ss...)
	return newEnv(t, 999999, fb)
}

func nameArg(n string) map[string]any { return map[string]any{"name": n} }

func nameRecord(t *testing.T, e *env, ref string) (journal.Record, bool) {
	t.Helper()
	a := e.srv.local(codexID(ref))
	if a == nil {
		t.Fatalf("%s not served", ref)
	}
	return a.Journal().Latest(journal.Record{Type: journal.TypeName})
}

func TestRenameChangesAliasKeepsAgentID(t *testing.T) {
	e := renameEnv(t, map[string]string{"T-1": "orig"})
	e.mustOK(thread("T-1"), "list_agents", map[string]any{})
	e.mustOK(thread("T-1"), "send_message", sendArgs("x", "hi"))
	before := e.descriptor(codexID("T-1"))

	out := e.mustOK(thread("T-1"), "rename", nameArg("CrossTalk Reviewer 3"))
	if out["name"] != "CrossTalk Reviewer 3" || out["previous"] != "orig" {
		t.Fatalf("rename result %v", out)
	}
	after := e.descriptor(codexID("T-1"))
	if after.Name != "CrossTalk Reviewer 3" || after.AgentID != before.AgentID || after.InstanceID != before.InstanceID || after.UDSPath != before.UDSPath {
		t.Fatalf("rename changed more than the alias:\nbefore %+v\nafter  %+v", before, after)
	}
	if d, ok, _ := e.reg.Lookup("CrossTalk Reviewer 3"); !ok || d.AgentID != before.AgentID {
		t.Errorf("new name does not resolve: %+v %v", d, ok)
	}
	if _, ok, _ := e.reg.Lookup("orig"); ok {
		t.Error("old alias still resolves")
	}
	if r, ok := nameRecord(t, e, "T-1"); !ok || r.Name != "CrossTalk Reviewer 3" {
		t.Errorf("name record %+v %v", r, ok)
	}
}

func TestRenameNameHeldRejected(t *testing.T) {
	e := renameEnv(t, map[string]string{"A": "alpha", "B": "beta"})
	e.mustOK(thread("A"), "list_agents", map[string]any{})
	e.mustOK(thread("A"), "send_message", sendArgs("x", "1"))
	e.mustOK(thread("B"), "send_message", sendArgs("x", "2"))

	e.mustErr(thread("B"), "rename", nameArg("alpha"), CodeRejected, "name_held")
	if d := e.descriptor(codexID("B")); d.Name != "beta" {
		t.Errorf("alias after refused rename %q", d.Name)
	}
	if _, ok := nameRecord(t, e, "B"); ok {
		t.Error("refused rename journaled a name")
	}
	if d := e.descriptor(codexID("A")); d.Name != "alpha" {
		t.Errorf("holder lost its name: %q", d.Name)
	}
}

func TestRenameInvalidName(t *testing.T) {
	e := renameEnv(t, map[string]string{"T-1": "orig"})
	e.mustOK(thread("T-1"), "send_message", sendArgs("x", "1"))
	for _, n := range []string{"", strings.Repeat("a", 65), "a@b", "a\nb", "0123456789abcdef", "inbox:role"} {
		e.mustErr(thread("T-1"), "rename", nameArg(n), CodeRejected, "invalid_name")
	}
	if d := e.descriptor(codexID("T-1")); d.Name != "orig" {
		t.Errorf("alias after invalid names %q", d.Name)
	}
	if _, ok := nameRecord(t, e, "T-1"); ok {
		t.Error("invalid rename journaled a name")
	}
}

func TestRenameSameNameStillPins(t *testing.T) {
	e := renameEnv(t, map[string]string{"T-1": "orig"})
	out := e.mustOK(thread("T-1"), "rename", nameArg("orig"))
	if out["name"] != "orig" || out["previous"] != "orig" {
		t.Fatalf("rename result %v", out)
	}
	e.fb.SetSessions(Session{Harness: "codex", SessionRef: "T-1", Name: "auto title", Cwd: "/w"})
	e.mustOK(thread("T-1"), "send_message", sendArgs("x", "1"))
	if d := e.descriptor(codexID("T-1")); d.Name != "orig" {
		t.Errorf("harness title moved a pinned alias to %q", d.Name)
	}
}

func TestFollowRenameIgnoresHarnessNameWhilePinned(t *testing.T) {
	e := renameEnv(t, map[string]string{"T-1": "orig"})
	e.mustOK(thread("T-1"), "rename", nameArg("pinned"))
	for _, harnessName := range []string{"auto title", "another title", ""} {
		e.fb.SetSessions(Session{Harness: "codex", SessionRef: "T-1", Name: harnessName, Cwd: "/w"})
		out := e.mustOK(thread("T-1"), "send_message", sendArgs("x", "1"))
		if d := e.descriptor(codexID("T-1")); d.Name != "pinned" {
			t.Fatalf("harness name %q moved a pinned alias to %q", harnessName, d.Name)
		}
		if n, ok := out["notice"]; ok {
			t.Errorf("pinned session got a notice for harness name %q: %v", harnessName, n)
		}
	}
}

func TestFollowRenameIgnoresClaudeNameWhilePinned(t *testing.T) {
	e := newEnv(t, 4242, NewFakeBackend())
	writeClaudeSession(t, e.sessionDir, 4242, "c-1", "alpha", "/w/a")
	e.mustOK(nil, "rename", nameArg("pinned"))
	writeClaudeSession(t, e.sessionDir, 4242, "c-1", "beta", "/w/a")
	e.mustOK(nil, "send_message", sendArgs("x", "1"))
	if d := e.descriptor(claudeID("c-1")); d.Name != "pinned" {
		t.Errorf("claude /rename moved a pinned alias to %q", d.Name)
	}
}

func TestUnpinnedFollowRenameUnchanged(t *testing.T) {
	e := renameEnv(t, map[string]string{"T-1": "orig"})
	e.mustOK(thread("T-1"), "send_message", sendArgs("x", "1"))
	e.fb.SetSessions(Session{Harness: "codex", SessionRef: "T-1", Name: "auto title", Cwd: "/w"})
	e.mustOK(thread("T-1"), "send_message", sendArgs("x", "2"))
	if d := e.descriptor(codexID("T-1")); d.Name != "auto title" {
		t.Errorf("unpinned alias %q, want it to follow the harness", d.Name)
	}
}

func TestRenameUnboundRefused(t *testing.T) {
	e := newEnv(t, 999999, NewFakeBackend())
	for _, meta := range []map[string]any{nil, {"sessionId": "S-1"}, {"threadId": ""}} {
		e.mustErr(meta, "rename", nameArg("x"), CodeUnbound, "")
	}
	if ds, _ := e.reg.List(); len(ds) != 0 {
		t.Fatalf("unbound rename registered %+v", ds)
	}
}

func TestRenameCodexThreadsIndependent(t *testing.T) {
	e := renameEnv(t, map[string]string{"A": "a", "B": "b"})
	e.mustOK(thread("A"), "rename", nameArg("first"))
	e.mustOK(thread("B"), "rename", nameArg("second"))
	if d := e.descriptor(codexID("A")); d.Name != "first" {
		t.Errorf("A = %q", d.Name)
	}
	if d := e.descriptor(codexID("B")); d.Name != "second" {
		t.Errorf("B = %q", d.Name)
	}
	e.fb.SetSessions(Session{Harness: "codex", SessionRef: "B", Name: "retitled", Cwd: "/w"}, Session{Harness: "codex", SessionRef: "A", Name: "a", Cwd: "/w"})
	e.mustOK(thread("A"), "send_message", sendArgs("x", "1"))
	e.mustOK(thread("B"), "send_message", sendArgs("x", "1"))
	if e.descriptor(codexID("A")).Name != "first" || e.descriptor(codexID("B")).Name != "second" {
		t.Error("a pin leaked across threads")
	}
}

func TestRenameSubagentAllowed(t *testing.T) {
	e := claimEnv(t)
	e.mustOK(thread("A"), "claim", map[string]any{"name": "b"})
	out := e.mustOK(thread("B"), "rename", nameArg("worker"))
	if out["name"] != "worker" {
		t.Fatalf("rename result %v", out)
	}
	if d := e.descriptor(codexID("B")); d.Name != "worker" || d.Role != registry.RoleSubagent {
		t.Errorf("descriptor %+v", d)
	}
}

func TestRenameJournalFailureRollsBack(t *testing.T) {
	e := renameEnv(t, map[string]string{"T-1": "orig"})
	e.mustOK(thread("T-1"), "send_message", sendArgs("x", "1"))
	e.srv.local(codexID("T-1")).Journal().Close()

	e.mustErr(thread("T-1"), "rename", nameArg("wanted"), CodeFailed, "")
	if d := e.descriptor(codexID("T-1")); d.Name != "orig" {
		t.Errorf("alias after failed append %q", d.Name)
	}
	g, err := registry.New(e.stateDir).Acquire(registry.Request{AgentID: "ffffffffffffffff", HarnessName: "wanted", Harness: "codex", Cwd: "/w"})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if g.Name() != "wanted" {
		t.Errorf("rolled-back name still locked: other registration got %q", g.Name())
	}
}
