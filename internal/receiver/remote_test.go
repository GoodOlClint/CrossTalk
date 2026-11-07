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
