package receiver

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/msg"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

var (
	fpA        = strings.Repeat("a", 64)
	fpB        = strings.Repeat("b", 64)
	remoteAl   = Sender{Verified: true, Host: fpA, HostAlias: "desk", AgentID: alice.AgentID, Name: "alice@desk", Role: registry.RolePrimary, Harness: "claude", Principal: RemoteKey(fpA, alice.AgentID)}
	remoteAlOn = Sender{Verified: true, Host: fpB, HostAlias: "lab", AgentID: alice.AgentID, Name: "alice@lab", Role: registry.RolePrimary, Harness: "claude", Principal: RemoteKey(fpB, alice.AgentID)}
)

func TestRemoteSenderIdentityShape(t *testing.T) {
	if remoteAl.key() != RemoteKey(fpA, alice.AgentID) || remoteAl.key() == alice.key() || remoteAl.registered() || remoteAl.label() != "alice@desk" {
		t.Fatalf("key %q registered %v label %q", remoteAl.key(), remoteAl.registered(), remoteAl.label())
	}
	if alice.key() != alice.AgentID || localUser.key() != PrincipalLocalUser || unverified.key() != unverifiedKey {
		t.Fatalf("local keys changed: %q %q %q", alice.key(), localUser.key(), unverified.key())
	}
}

func TestRemoteRouteRecordsHostScopedKey(t *testing.T) {
	e := newEnv(t)
	local := newMsg(msg.Ask, "local")
	if res := e.recv(A2A, alice, local); res.State != adapter.Accepted {
		t.Fatalf("%+v", res)
	}
	m := newMsg(msg.Ask, "remote")
	if res := e.recv(A2A, remoteAl, m); res.State != adapter.Accepted {
		t.Fatalf("%+v", res)
	}
	if !e.r.RouteOpen(local.MessageID) || !e.r.RouteOpen(m.MessageID) {
		t.Fatal("a route is missing")
	}
	routes := map[string]string{}
	rs, _ := e.j.Records()
	for _, r := range rs {
		if r.Type == journal.TypeRoute && r.State == journal.StateOpen {
			routes[r.MessageID] = r.Sender
		}
	}
	if routes[local.MessageID] != alice.AgentID || routes[m.MessageID] != RemoteKey(fpA, alice.AgentID) {
		t.Fatalf("routes %v", routes)
	}
	if w := e.fake.Writes(); len(w) != 2 || w[1].Message.From != "alice@desk" {
		t.Fatalf("writes %+v", w)
	}
}

func TestRemoteReceiptUsesHostScopedKey(t *testing.T) {
	e := newEnv(t)
	ask := newMsg(msg.Ask, "q")
	key := RemoteKey(fpA, alice.AgentID)
	body, _ := json.Marshal(ask)
	if err := e.j.Append(journal.Record{Type: journal.TypeOutbound, MessageID: ask.MessageID, To: key, Kind: string(msg.Ask), ContextID: ask.ContextID, State: "accepted", Message: body}); err != nil {
		t.Fatal(err)
	}
	reply := func(s Sender) msg.Message {
		m := newMsg(msg.Inform, "a")
		m.InReplyTo, m.ContextID = &ask.MessageID, ask.ContextID
		if res := e.recv(A2A, s, m); res.State != adapter.Accepted {
			t.Fatalf("%+v", res)
		}
		return m
	}
	receipts := func() (n int) {
		rs, _ := e.j.Records()
		for _, r := range rs {
			if r.Type == journal.TypeReceipt && r.InReplyTo == ask.MessageID {
				n++
			}
		}
		return
	}
	reply(alice)
	reply(remoteAlOn)
	if n := receipts(); n != 0 {
		t.Fatalf("a local agent or another host with the same agent_id recorded %d receipts for an ask to host A", n)
	}
	reply(remoteAl)
	if n := receipts(); n != 1 {
		t.Fatalf("the asked host's reply recorded %d receipts, want 1", n)
	}
}

func TestRemoteNeverPassesLocalOnlyPaths(t *testing.T) {
	e := newEnv(t)
	selfAsRemote := remoteAl
	selfAsRemote.AgentID = selfID
	if res := e.native(selfAsRemote, "hi"); res.State == adapter.Bounced {
		t.Fatalf("a remote sender with this agent's own agent_id took the native-reply bounce path: %+v", res)
	}
	m := newMsg(msg.Command, "run")
	if res := e.recv(A2A, remoteAl, m); res.State != adapter.Rejected || res.Reason != ReasonRemoteKind {
		t.Fatalf("remote command %+v", res)
	}
}

func TestRemoteHostsHaveSeparateNamespaces(t *testing.T) {
	e := newEnv(t)
	for i, s := range []Sender{alice, remoteAl, remoteAlOn} {
		m := newMsg(msg.Inform, "own id")
		if res := e.recv(A2A, s, m); res.State != adapter.Accepted || res.Duplicate {
			t.Fatalf("%s: %+v", s.key(), res)
		}
		if st, ok := e.j.Dedupe(s.key(), m.MessageID); !ok || st != "accepted" {
			t.Fatalf("%d: record under %s: %q %v", i, s.key(), st, ok)
		}
	}
	m := newMsg(msg.Inform, "contested id")
	if res := e.recv(A2A, alice, m); res.State != adapter.Accepted {
		t.Fatalf("%+v", res)
	}
	for _, s := range []Sender{remoteAl, remoteAlOn} {
		if res := e.recv(A2A, s, m); res.State != adapter.Rejected || res.Reason != ReasonIDInUse {
			t.Fatalf("%s reusing a message_id held by another sender: %+v", s.key(), res)
		}
		if _, ok := e.j.Dedupe(s.key(), m.MessageID); ok {
			t.Fatalf("a refused message left a record under %s", s.key())
		}
	}
	if res := e.recv(A2A, alice, m); !res.Duplicate {
		t.Fatalf("a true duplicate from the same sender: %+v", res)
	}
	if n := e.writes(); n != 4 {
		t.Fatalf("injects %d, want 4", n)
	}
}

func TestRecoverScopesRemoteDispatch(t *testing.T) {
	e := newEnv(t)
	id := msg.NewUUIDv7()
	key := RemoteKey(fpA, alice.AgentID)
	if _, _, err := e.j.Accept(key, id, journal.StateSubmitted); err != nil {
		t.Fatal(err)
	}
	if ok, err := e.j.ClaimFor(key, id); !ok || err != nil {
		t.Fatalf("claim %v %v", ok, err)
	}
	e.j.Close()
	rp := e.open(selfBinding, "inst-2")
	if err := e.r.Recover(rp); err != nil {
		t.Fatal(err)
	}
	if l, ok := e.j.DispatchStateFor(key, id); !ok || l.Type != journal.TypeOutcome || l.State != string(adapter.Unknown) {
		t.Fatalf("remote dispatch %+v %v", l, ok)
	}
	if st, _ := e.j.Dedupe(key, id); st != string(adapter.Unknown) {
		t.Fatalf("remote dedupe state %q", st)
	}
	if _, claimed := e.j.DispatchState(id); claimed {
		t.Fatal("the remote claim leaked into the local namespace")
	}
}
