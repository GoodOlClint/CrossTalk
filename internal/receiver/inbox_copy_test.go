package receiver

import (
	"context"
	"testing"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/msg"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

func (e *env) recvIn(s Sender, m msg.Message, inbox string, copied bool) Result {
	return e.r.Receive(context.Background(), Inbound{Ingress: A2A, Message: m, Sender: s, Inbox: inbox, InboxCopy: copied})
}

func dedupeFor(t *testing.T, e *env, sender, id string) journal.Record {
	t.Helper()
	rs, err := e.j.Records()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rs {
		if r.Type == journal.TypeDedupe && r.Sender == sender && r.MessageID == id && len(r.Message) > 0 {
			return r
		}
	}
	t.Fatalf("no dedupe record for %s", id)
	return journal.Record{}
}

func TestInboxCopyOpensNoRoute(t *testing.T) {
	for _, kind := range []msg.Kind{msg.Inform, msg.Ask} {
		e := newEnv(t)
		m := newMsg(kind, "copy")
		res := e.recvIn(alice, m, "coord", true)
		if res.State != adapter.Accepted {
			t.Fatalf("%s copy: %+v", kind, res)
		}
		if e.r.RouteOpen(m.MessageID) {
			t.Fatalf("%s copy opened a route", kind)
		}
		rs, _ := e.j.Records()
		for _, r := range rs {
			if r.Type == journal.TypeRoute && r.MessageID == m.MessageID {
				t.Fatalf("%s copy journaled a route: %+v", kind, r)
			}
		}
		w := e.fake.Writes()
		if len(w) != 1 || w[0].Delivery.Inbox != "coord" || w[0].Message.Kind != kind {
			t.Fatalf("%s copy writes %+v", kind, w)
		}
		if d := dedupeFor(t, e, alice.AgentID, m.MessageID); !d.Copy || d.Inbox != "coord" {
			t.Fatalf("dedupe record %+v", d)
		}
	}

	e := newEnv(t)
	own := newMsg(msg.Ask, "responder")
	if res := e.recvIn(alice, own, "coord", false); res.State != adapter.Accepted || !e.r.RouteOpen(own.MessageID) {
		t.Fatalf("responder leg: %+v route=%v", res, e.r.RouteOpen(own.MessageID))
	}
	if w := e.fake.Writes(); w[0].Delivery.Inbox != "coord" {
		t.Fatalf("responder leg does not show the inbox: %+v", w[0].Delivery)
	}
	if d := dedupeFor(t, e, alice.AgentID, own.MessageID); d.Copy || d.Inbox != "coord" {
		t.Fatalf("responder dedupe record %+v", d)
	}
}

func TestInboxForgedCopyFlagOnlyRemovesRoute(t *testing.T) {
	for _, kind := range []msg.Kind{msg.Inform, msg.Ask, msg.Command} {
		for _, s := range []Sender{alice, sub, localUser, unverified} {
			plain, forged := newEnv(t), newEnv(t)
			a := plain.recvIn(s, newMsg(kind, "x"), "", false)
			b := forged.recvIn(s, newMsg(kind, "x"), "", true)
			if a.State != b.State || a.Reason != b.Reason {
				t.Errorf("%s from %s: plain %+v forged %+v", kind, s.label(), a, b)
			}
		}
	}
	e := newEnv(t)
	m := newMsg(msg.Inform, "x")
	e.recvIn(alice, m, "", true)
	if e.r.RouteOpen(m.MessageID) {
		t.Fatal("forged flag kept a route")
	}
}

func TestInboxCopyAuthorizedAgainstOriginalKind(t *testing.T) {
	e := newEnv(t)
	ask := newMsg(msg.Ask, "q")
	if res := e.recvIn(unverified, ask, "coord", true); res.State != adapter.Rejected || res.Reason != ReasonPolicy {
		t.Fatalf("unverified ask copy: %+v", res)
	}
	if e.writes() != 0 {
		t.Fatal("rejected copy reached the harness")
	}
	cmd := newMsg(msg.Command, "do")
	e.setRole(registry.RolePrimary, "")
	if res := e.recvIn(alice, cmd, "coord", true); res.State != adapter.Accepted {
		t.Fatalf("authorized command copy: %+v", res)
	}
	e.setRole(registry.RoleSubagent, "someone-else")
	if res := e.recvIn(alice, newMsg(msg.Command, "do"), "coord", true); res.State != adapter.Rejected || res.Reason != ReasonPolicy {
		t.Fatalf("command copy to a subagent not owned by the sender: %+v", res)
	}
	e.setRole(registry.RolePrimary, "")
	if res := e.recvIn(alice, newMsg(msg.Ask, "q"), "coord", true); res.State != adapter.Accepted {
		t.Fatalf("verified ask copy: %+v", res)
	}
}

func TestDeliveryCarriesInboxCopyFlag(t *testing.T) {
	for _, copied := range []bool{false, true} {
		e := newEnv(t)
		m := newMsg(msg.Ask, "x")
		if res := e.recvIn(alice, m, "coord", copied); res.State != adapter.Accepted {
			t.Fatalf("%+v", res)
		}
		if w := e.fake.Writes(); len(w) != 1 || w[0].Delivery.InboxCopy != copied {
			t.Fatalf("copy=%v writes %+v", copied, w)
		}
	}
}
