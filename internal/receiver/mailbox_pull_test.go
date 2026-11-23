package receiver

import (
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/msg"
)

func newMailboxClockEnv(t *testing.T) (*env, *clock) {
	e, c := newClockEnv(t)
	e.fake.SetDefaultInject(adapter.Outcome{State: adapter.Mailboxed})
	return e, c
}

func TestPull_FirstPullAfterTTL_ExpectReplyRouteOpen(t *testing.T) {
	e, c := newMailboxClockEnv(t)
	m := e.mailboxed("fyi")
	c.advance(InformRouteTTL + 30*time.Minute)
	e.pull("", 0)
	if !e.r.RouteOpen(m.MessageID) {
		t.Fatal("reply route expired before its first pull")
	}
}

func TestPull_RouteStillExpiresOneTTLAfterPull(t *testing.T) {
	e, c := newMailboxClockEnv(t)
	m := e.mailboxed("fyi")
	c.advance(InformRouteTTL + 30*time.Minute)
	e.pull("", 0)
	c.advance(InformRouteTTL - time.Minute)
	if !e.r.RouteOpen(m.MessageID) {
		t.Fatal("route closed before one TTL after the pull")
	}
	c.advance(2 * time.Minute)
	if e.r.RouteOpen(m.MessageID) {
		t.Fatal("route outlived one TTL after the pull")
	}
}

func TestPull_RouteWindowSurvivesRestart(t *testing.T) {
	e, c := newMailboxClockEnv(t)
	m := e.mailboxed("fyi")
	c.advance(InformRouteTTL + 30*time.Minute)
	e.pull("", 0)
	c.advance(30 * time.Minute)
	e.j.Close()
	rp := e.open(selfBinding, "inst-2")
	e.r = New(Config{Journal: e.j, Adapter: e.fake, Self: e.self, Now: c.now})
	if err := e.r.Recover(rp); err != nil {
		t.Fatal(err)
	}
	if !e.r.RouteOpen(m.MessageID) {
		t.Fatal("route window did not survive a restart after the pull")
	}
	c.advance(InformRouteTTL)
	if e.r.RouteOpen(m.MessageID) {
		t.Fatal("restored route outlived one TTL after the pull")
	}
}

func TestPull_ExpiredRouteSweptBeforePull_ExpectReopened(t *testing.T) {
	e, c := newMailboxClockEnv(t)
	m := e.mailboxed("fyi")
	c.advance(InformRouteTTL + time.Minute)
	if e.r.RouteOpen(m.MessageID) {
		t.Fatal("setup: route should have expired and been swept")
	}
	e.pull("", 0)
	if !e.r.RouteOpen(m.MessageID) {
		t.Fatal("swept route not reopened by the first pull")
	}
}

func TestPull_InboxCopy_ExpectNoRouteOpened(t *testing.T) {
	e, _ := newMailboxClockEnv(t)
	m := newMsg(msg.Inform, "copy")
	if res := e.recvIn(alice, m, "coord", true); res.State != adapter.Mailboxed {
		t.Fatalf("%+v", res)
	}
	e.pull("", 0)
	if e.r.RouteOpen(m.MessageID) {
		t.Fatal("pulling a watcher copy opened a reply route")
	}
}

func TestPull_CarriesInboxName(t *testing.T) {
	e := newMailboxEnv(t)
	resp, copied := newMsg(msg.Inform, "resp"), newMsg(msg.Inform, "copy")
	if res := e.recvIn(alice, resp, "coord", false); res.State != adapter.Mailboxed {
		t.Fatalf("%+v", res)
	}
	if res := e.recvIn(alice, copied, "coord", true); res.State != adapter.Mailboxed {
		t.Fatalf("%+v", res)
	}
	es, _ := e.pull("", 0)
	if len(es) != 2 || es[0].Inbox != "coord" || es[0].InboxCopy || es[1].Inbox != "coord" || !es[1].InboxCopy {
		t.Fatalf("pull %+v", es)
	}
}

func TestPull_AskRouteUnchangedByPull(t *testing.T) {
	e, c := newMailboxClockEnv(t)
	m := newMsg(msg.Ask, "q")
	if res := e.recv(A2A, alice, m); res.State != adapter.Mailboxed {
		t.Fatalf("%+v", res)
	}
	c.advance(3 * InformRouteTTL)
	e.pull("", 0)
	c.advance(3 * InformRouteTTL)
	if !e.r.RouteOpen(m.MessageID) {
		t.Fatal("ask route closed by time or by a pull")
	}
}

func TestPull_WatcherCopyAfterWindow_ExpectNoRoute(t *testing.T) {
	e, c := newMailboxClockEnv(t)
	m := newMsg(msg.Inform, "copy")
	if res := e.recvIn(alice, m, "coord", true); res.State != adapter.Mailboxed {
		t.Fatalf("%+v", res)
	}
	c.advance(InformRouteTTL + time.Hour)
	e.pull("", 0)
	if e.r.RouteOpen(m.MessageID) || e.closeRoute(m.MessageID) {
		t.Fatal("watcher copy has a reply route after its pull")
	}
}

func TestPull_AskAppendsNoRouteRecord(t *testing.T) {
	e, _ := newMailboxClockEnv(t)
	m := newMsg(msg.Ask, "q")
	e.recv(A2A, alice, m)
	before := len(routeRecords(t, e, m.MessageID))
	e.pull("", 0)
	if n := len(routeRecords(t, e, m.MessageID)); n != before {
		t.Fatalf("pull appended %d route records for an ask", n-before)
	}
}

func TestPull_RouteReopenAppendFails_ExpectOldRouteKeptAndEntryMailboxed(t *testing.T) {
	e := newMailboxEnv(t)
	var arm bool
	l := &appendLog{fail: func(r journal.Record) bool { return arm && r.Type == journal.TypeRoute && r.State == journal.StateOpen }}
	e.openLogged(l)
	m := e.mailboxed("fyi")
	arm = true
	if _, _, err := e.r.Pull("", 0); err == nil {
		t.Fatal("pull succeeded though the route reopen failed")
	}
	if !e.r.RouteOpen(m.MessageID) || e.dedupe(m) != "mailboxed" {
		t.Fatalf("route open %v, state %q", e.r.RouteOpen(m.MessageID), e.dedupe(m))
	}
}

func TestPull_EntryFromOtherConversation_ExpectRouteStaysClosed(t *testing.T) {
	e := newMailboxEnv(t)
	m := e.mailboxed("fyi")
	e.j.Close()
	other := selfBinding
	other.HarnessEndpoint = "/tmp/cc-socks/2.sock"
	rp := e.open(other, "inst-2")
	if err := e.r.Recover(rp); err != nil {
		t.Fatal(err)
	}
	e.pull("", 0)
	if e.r.RouteOpen(m.MessageID) {
		t.Fatal("pull reopened a route that recovery closed for another conversation")
	}
}
