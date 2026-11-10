package mcpserver

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/identity"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

func publishAgent(t *testing.T, reg *registry.Registry, ref, name string) *registry.Registration {
	t.Helper()
	g, err := reg.Acquire(registry.Request{AgentID: identity.AgentID("codex", ref), HarnessName: name, Harness: "codex", Cwd: "/w"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.Close() })
	if err := g.Publish(registry.Descriptor{SessionRef: ref}); err != nil {
		t.Fatal(err)
	}
	return g
}

func TestInboxAddressNeverMatchesSessionName(t *testing.T) {
	reg := registry.New(t.TempDir())
	s := publishAgent(t, reg, "named", "inbox:x").Descriptor()
	calls := 0
	discover := func(context.Context) ([]Session, error) {
		calls++
		return []Session{{Harness: "codex", SessionRef: "native", Name: "inbox:x"}}, nil
	}
	for _, to := range []string{"inbox:x", "INBOX:x"} {
		tg, err := ResolveTarget(context.Background(), reg, discover, nil, to)
		if err != nil {
			t.Fatal(err)
		}
		if tg.Registered || len(tg.Native) != 0 || tg.Inbox == nil || tg.Inbox.Name != "x" {
			t.Fatalf("%s resolved as a session: %+v", to, tg)
		}
		if tg.Rejection == nil || tg.Rejection.Reason != "no_such_agent" {
			t.Fatalf("%s with no member: %+v", to, tg.Rejection)
		}
	}
	if calls != 0 {
		t.Fatalf("discover called %d times for an inbox address", calls)
	}
	tg, err := ResolveTarget(context.Background(), reg, discover, nil, s.AgentID)
	if err != nil || !tg.Registered || tg.Desc.AgentID != s.AgentID || tg.Inbox != nil {
		t.Fatalf("agent_id of the session named inbox:x: %+v %v", tg, err)
	}
	g1, g2 := publishAgent(t, reg, "m1", "one"), publishAgent(t, reg, "m2", "two")
	nop := func(registry.InboxEvent) error { return nil }
	if _, err := reg.AttachInbox(g1, "x", nop); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.GrantInbox(g1, "x", g2.Descriptor().AgentID, false); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	if _, err := reg.AttachInbox(g2, "x", nop); err != nil {
		t.Fatal(err)
	}
	m1, m2 := g1.Descriptor(), g2.Descriptor()
	tg, err = ResolveTarget(context.Background(), reg, discover, nil, "Inbox:X")
	if err != nil || tg.Rejection != nil || tg.Registered {
		t.Fatalf("inbox with members: %+v %v", tg, err)
	}
	if len(tg.Inbox.Members) != 2 || tg.Inbox.Members[0].AgentID != m2.AgentID || tg.Inbox.Members[1].AgentID != m1.AgentID {
		t.Fatalf("members %+v, want responder first", tg.Inbox.Members)
	}
}

func TestInboxRemoteAddressRejected(t *testing.T) {
	reg := registry.New(t.TempDir())
	calls := 0
	discover := func(context.Context) ([]Session, error) { calls++; return nil, nil }
	for _, to := range []string{"inbox:x@lab", "INBOX:x@lab", "inbox:x@nowhere"} {
		tg, err := ResolveTarget(context.Background(), reg, discover, func(string) bool { return true }, to)
		if err != nil {
			t.Fatal(err)
		}
		if tg.Rejection == nil || tg.Rejection.Reason != "remote_inbox_unsupported" || tg.Remote != nil || tg.UnknownAlias != "" {
			t.Fatalf("%s: %+v", to, tg)
		}
	}
	if calls != 0 {
		t.Fatalf("discover called %d times", calls)
	}
	if hintFor("remote_inbox_unsupported") == "" {
		t.Error("no hint for remote_inbox_unsupported")
	}
}

func TestInboxZeroMembersNoSuchAgent(t *testing.T) {
	reg := registry.New(t.TempDir())
	if _, err := reg.AttachInbox(publishAgent(t, reg, "m1", "one"), "other", func(registry.InboxEvent) error { return nil }); err != nil {
		t.Fatal(err)
	}
	tg, err := ResolveTarget(context.Background(), reg, func(context.Context) ([]Session, error) { return nil, nil }, nil, "inbox:x")
	if err != nil {
		t.Fatal(err)
	}
	r := tg.Rejection
	if r == nil || r.Code != CodeRejected || r.Reason != "no_such_agent" || !strings.Contains(r.Hint, "inbox") || !strings.Contains(r.Hint, "attach") {
		t.Fatalf("rejection %+v", r)
	}
	if r.Hint == hintFor("no_such_agent") {
		t.Error("zero-member hint is the generic session hint")
	}
	tg, _ = ResolveTarget(context.Background(), reg, func(context.Context) ([]Session, error) { return nil, nil }, nil, "inbox:")
	if tg.Rejection == nil || tg.Rejection.Reason != "invalid_inbox" {
		t.Fatalf("empty inbox name: %+v", tg.Rejection)
	}
}

func TestListAgentsShowsInboxOwnerAndResponder(t *testing.T) {
	e := claimEnv(t)
	A, B := thread("A"), thread("B")
	e.mustOK(A, "attach", inboxArg("coord"))
	e.mustOK(A, "grant", grantArgs("coord", "b", false))
	e.mustOK(B, "attach", inboxArg("coord"))
	e.mustOK(A, "attach", inboxArg("solo"))

	rows := rowsByID(t, e.mustOK(nil, "list_agents", map[string]any{}))
	inboxes := func(id string) map[string]map[string]any {
		out := map[string]map[string]any{}
		list, _ := rows[id]["inboxes"].([]any)
		for _, x := range list {
			m := x.(map[string]any)
			out[m["name"].(string)] = m
		}
		return out
	}
	a, b := inboxes(codexID("A")), inboxes(codexID("B"))
	if a["coord"]["owner"] != true || a["coord"]["responder"] != nil || a["solo"]["owner"] != true || a["solo"]["responder"] != true {
		t.Errorf("A rows %v", a)
	}
	if b["coord"]["owner"] != nil || b["coord"]["responder"] != true {
		t.Errorf("B rows %v", b)
	}
	if _, has := rows[codexID("C")]["inboxes"]; has {
		t.Errorf("row without inbox has the key: %v", rows[codexID("C")])
	}
}
