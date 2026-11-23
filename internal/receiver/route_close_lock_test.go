package receiver

import (
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/journal"
)

func (e *env) closeRoute(id string) bool {
	e.t.Helper()
	ok, err := e.r.CloseRoute(id)
	if err != nil {
		e.t.Fatal(err)
	}
	return ok
}

// pullDuringClose arms beforeRouteClose so the close of id starts a Pull and
// waits up to 200ms for it. The returned func waits for that Pull and fails
// the test unless it returned id with no error.
func pullDuringClose(e *env, id string) func() {
	done := make(chan struct{})
	var es []Entry
	var err error
	armed := true
	e.r.beforeRouteClose = func(mid string) {
		if !armed || mid != id {
			return
		}
		armed = false
		go func() {
			defer close(done)
			es, _, err = e.r.Pull("", 0)
		}()
		select {
		case <-done:
		case <-time.After(200 * time.Millisecond):
		}
	}
	return func() {
		e.t.Helper()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			e.t.Fatal("pull did not finish")
		}
		if err != nil || len(es) != 1 || es[0].MessageID != id {
			e.t.Fatalf("pull %+v, %v", es, err)
		}
	}
}

func journalRouteOpen(t *testing.T, e *env, id string) bool {
	t.Helper()
	rs := routeRecords(t, e, id)
	return len(rs) > 0 && rs[len(rs)-1].State == journal.StateOpen
}

func TestCloseRoute_RacesFirstPull_ExpectJournalMatchesMemory(t *testing.T) {
	e, _ := newMailboxClockEnv(t)
	m := e.mailboxed("fyi")
	wait := pullDuringClose(e, m.MessageID)
	if !e.closeRoute(m.MessageID) {
		t.Fatal("no route to close")
	}
	wait()
	if st := e.dedupe(m); st != journal.StateDelivered {
		t.Fatalf("entry %q after the pull", st)
	}
	mem, jr := e.r.RouteOpen(m.MessageID), journalRouteOpen(t, e, m.MessageID)
	if mem != jr {
		t.Fatalf("route open in memory %v, in journal %v", mem, jr)
	}
	if mem {
		t.Fatal("pull reopened a route a reply closed")
	}
}

func TestPull_RouteClosedByReplyBeforeFirstPull_ExpectNotReopened(t *testing.T) {
	e, _ := newMailboxClockEnv(t)
	m := e.mailboxed("fyi")
	if !e.closeRoute(m.MessageID) {
		t.Fatal("no route to close")
	}
	e.pull("", 0)
	if e.r.RouteOpen(m.MessageID) || journalRouteOpen(t, e, m.MessageID) {
		t.Fatal("first pull reopened a route a reply closed")
	}
}

func TestPull_ReplyCloseAtTTLEdge_ExpectNotReopened(t *testing.T) {
	e, c := newMailboxClockEnv(t)
	m := e.mailboxed("fyi")
	c.advance(InformRouteTTL)
	c.setStep(time.Millisecond)
	if !e.closeRoute(m.MessageID) {
		t.Fatal("route expired before the reply closed it")
	}
	c.setStep(0)
	e.pull("", 0)
	if e.r.RouteOpen(m.MessageID) {
		t.Fatal("first pull took a reply close at the TTL edge for an expiry")
	}
}

func TestCloseRoute_AppendFails_ExpectErrorAndRouteKept(t *testing.T) {
	e, _ := newMailboxClockEnv(t)
	var arm bool
	l := &appendLog{fail: func(r journal.Record) bool { return arm && isRouteClosed(r) }}
	e.openLogged(l)
	m := e.mailboxed("fyi")
	arm = true
	if ok, err := e.r.CloseRoute(m.MessageID); !ok || err == nil {
		t.Fatalf("CloseRoute %v, %v; want true and the append error", ok, err)
	}
	if !e.r.RouteOpen(m.MessageID) || !journalRouteOpen(t, e, m.MessageID) {
		t.Fatal("route dropped though its close was not written")
	}
}

func TestSweep_RacesFirstPull_ExpectJournalMatchesMemory(t *testing.T) {
	e, c := newMailboxClockEnv(t)
	m := e.mailboxed("fyi")
	c.advance(InformRouteTTL + time.Minute)
	wait := pullDuringClose(e, m.MessageID)
	e.r.RouteOpen(m.MessageID)
	wait()
	mem, jr := e.r.RouteOpen(m.MessageID), journalRouteOpen(t, e, m.MessageID)
	if mem != jr {
		t.Fatalf("route open in memory %v, in journal %v", mem, jr)
	}
	if !mem {
		t.Fatal("first pull did not reopen the expired route")
	}
}
