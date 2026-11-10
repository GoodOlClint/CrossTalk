package mcpserver

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/GoodOlClint/crosstalk/internal/journal"
)

func inboxArg(n string) map[string]any { return map[string]any{"inbox": n} }

func grantArgs(inbox, to string, owner bool) map[string]any {
	return map[string]any{"inbox": inbox, "to": to, "owner": owner}
}

func TestAttachToolResultCarriesCreatedAndOwner(t *testing.T) {
	e := claimEnv(t)
	A, B, C := thread("A"), thread("B"), thread("C")

	out := e.mustOK(A, "attach", inboxArg("coord"))
	if out["inbox"] != "coord" || out["created"] != true || out["owner"] != codexID("A") {
		t.Fatalf("attach result %v", out)
	}
	if ms, _ := out["members"].([]any); len(ms) != 1 || ms[0].(map[string]any)["agent_id"] != codexID("A") {
		t.Fatalf("members %v", out["members"])
	}
	if r, ok := e.srv.local(codexID("A")).Journal().Latest(journal.Record{Type: journal.TypeAttach, Inbox: "coord"}); !ok || !r.Attached || r.At.IsZero() {
		t.Fatalf("attach not journaled: %+v %v", r, ok)
	}
	if out := e.mustOK(A, "attach", inboxArg("coord")); out["created"] != false {
		t.Fatalf("re-attach %v", out)
	}

	e.mustErr(B, "attach", inboxArg("coord"), CodeRejected, "not_granted")
	g := e.mustOK(A, "grant", grantArgs("coord", "b", false))
	if gr, _ := g["grants"].([]any); g["inbox"] != "coord" || g["owner"] != codexID("A") || len(gr) != 1 || gr[0] != codexID("B") {
		t.Fatalf("grant result %v", g)
	}
	out = e.mustOK(B, "attach", inboxArg("coord"))
	if ms, _ := out["members"].([]any); out["created"] != false || len(ms) != 2 || ms[0].(map[string]any)["agent_id"] != codexID("B") {
		t.Fatalf("second attach %v", out)
	}
	e.mustErr(B, "grant", grantArgs("coord", "c", false), CodeRejected, "not_owner")

	out = e.mustOK(A, "detach", inboxArg("coord"))
	if _, has := out["created"]; has || out["owner"] != nil {
		t.Fatalf("owner detach %v", out)
	}
	if r, _ := e.srv.local(codexID("A")).Journal().Latest(journal.Record{Type: journal.TypeAttach, Inbox: "coord"}); r.Attached {
		t.Fatalf("detach not journaled: %+v", r)
	}
	e.mustErr(C, "attach", inboxArg("coord"), CodeRejected, "ownerless")
	e.mustErr(C, "claim", map[string]any{"name": "inbox:ghost"}, CodeRejected, "no_such_inbox")
	out = e.mustOK(C, "claim", map[string]any{"name": "inbox:coord"})
	if out["inbox"] != "coord" || out["owner"] != codexID("C") {
		t.Fatalf("claim result %v", out)
	}
	e.mustErr(thread("A"), "claim", map[string]any{"name": "inbox:coord"}, CodeRejected, "inbox_owned")
	e.mustOK(A, "attach", inboxArg("coord"))
	if _, out := e.call(C, "grant", grantArgs("coord", "a", true)); out["owner"] != codexID("A") {
		t.Fatalf("hand over %v", out)
	}
	e.mustErr(C, "grant", grantArgs("coord", "nobody", false), CodeRejected, "no_such_agent")
}

func TestInboxToolsRefuseSubagents(t *testing.T) {
	e := claimEnv(t)
	e.mustOK(thread("A"), "claim", map[string]any{"name": "b"})
	B := thread("B")
	e.mustErr(B, "attach", inboxArg("coord"), CodeRejected, "caller_not_primary")
	e.mustErr(B, "detach", inboxArg("coord"), CodeRejected, "caller_not_primary")
	e.mustErr(B, "grant", grantArgs("coord", "c", false), CodeRejected, "caller_not_primary")
	e.mustErr(B, "claim", map[string]any{"name": "inbox:coord"}, CodeRejected, "caller_not_primary")
	if ds, _ := e.reg.InboxMembers("coord"); len(ds) != 0 {
		t.Fatalf("subagent joined: %v", ds)
	}
}

func TestInboxActionsSelfOnlyAndUnbound(t *testing.T) {
	e := claimEnv(t)
	for tool, args := range map[string]any{"attach": inboxArg("x"), "detach": inboxArg("x"), "grant": grantArgs("x", "b", false), "claim": map[string]any{"name": "inbox:x"}} {
		e.mustErr(nil, tool, args, CodeUnbound, "")
	}
	res, err := e.cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	props := map[string][]string{"attach": {"inbox"}, "detach": {"inbox"}, "grant": {"inbox", "owner", "to"}}
	for _, tl := range res.Tools {
		want, ok := props[tl.Name]
		if !ok {
			continue
		}
		b, _ := json.Marshal(tl.InputSchema)
		var s struct {
			Properties map[string]any `json:"properties"`
		}
		json.Unmarshal(b, &s)
		var got []string
		for k := range s.Properties {
			got = append(got, k)
		}
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("%s properties %v, want %v", tl.Name, got, want)
		}
	}
	e.mustOK(thread("A"), "attach", inboxArg("x"))
	e.mustErr(thread("B"), "grant", grantArgs("x", "c", false), CodeRejected, "not_owner")
	e.mustOK(thread("B"), "detach", inboxArg("x"))
	if ds, _ := e.reg.InboxMembers("x"); len(ds) != 1 || ds[0].AgentID != codexID("A") {
		t.Fatalf("a peer's detach changed membership: %v", ds)
	}
}

func TestClaimInboxFormDoesNotResolveASession(t *testing.T) {
	fb := NewFakeBackend()
	fb.SetSessions(
		Session{Harness: "codex", SessionRef: "A", Name: "a", Cwd: "/w"},
		Session{Harness: "codex", SessionRef: "G", Name: "inbox:ghost", Cwd: "/w"},
	)
	e := newEnv(t, 999999, fb)
	e.mustOK(thread("A"), "list_agents", map[string]any{})
	e.mustOK(thread("G"), "list_agents", map[string]any{})
	e.mustErr(thread("A"), "claim", map[string]any{"name": "inbox:ghost"}, CodeRejected, "no_such_inbox")
	e.mustErr(thread("A"), "claim", map[string]any{"name": "INBOX:ghost"}, CodeRejected, "no_such_inbox")
	if d := e.descriptor(codexID("G")); d.Role != "primary" || d.Owner != nil {
		t.Fatalf("session claimed through the inbox form: %+v", d)
	}
	for _, c := range fb.Calls() {
		if c.Op == "claim" || c.Op == "send" {
			t.Fatalf("session was reached: %+v", c)
		}
	}
}

func TestReleaseInboxFormIsInvalidInbox(t *testing.T) {
	e := claimEnv(t)
	e.mustOK(thread("A"), "attach", inboxArg("coord"))
	for _, n := range []string{"inbox:coord", "INBOX:coord", "inbox:nothing"} {
		e.mustErr(thread("A"), "release", map[string]any{"name": n}, CodeRejected, "invalid_inbox")
	}
	if ds, _ := e.reg.InboxMembers("coord"); len(ds) != 1 {
		t.Fatalf("release changed membership: %v", ds)
	}
}

func TestInvalidInboxNames(t *testing.T) {
	e := claimEnv(t)
	A := thread("A")
	for _, n := range []string{"", "a@b", "a/b", "a\nb", "a\x00b", "inbox:x"} {
		e.mustErr(A, "attach", inboxArg(n), CodeRejected, "invalid_inbox")
		e.mustErr(A, "detach", inboxArg(n), CodeRejected, "invalid_inbox")
		e.mustErr(A, "grant", grantArgs(n, "b", false), CodeRejected, "invalid_inbox")
	}
	for _, n := range []string{"inbox:", "inbox:a@b", "inbox:a/b", "inbox:a\nb"} {
		e.mustErr(A, "claim", map[string]any{"name": n}, CodeRejected, "invalid_inbox")
	}
	if strings.Contains(describe(&Error{Code: CodeRejected, Reason: "ownerless"}), "claim") == false {
		t.Error("ownerless hint does not point at claim")
	}
}
