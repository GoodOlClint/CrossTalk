package receiver

import (
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/journal"
)

// pullDuringClose arms beforeRouteClose so the close of id starts a Pull and
// waits up to 200ms for it. It returns a wait func for the Pull's goroutine.
func pullDuringClose(e *env, id string) func() {
	done := make(chan struct{})
	armed := true
	e.r.beforeRouteClose = func(mid string) {
		if !armed || mid != id {
			return
		}
		armed = false
		go func() {
			defer close(done)
			e.r.Pull("", 0)
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
	if !e.r.CloseRoute(m.MessageID) {
		t.Fatal("no route to close")
	}
	wait()
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
	if !e.r.CloseRoute(m.MessageID) {
		t.Fatal("no route to close")
	}
	e.pull("", 0)
	if e.r.RouteOpen(m.MessageID) || journalRouteOpen(t, e, m.MessageID) {
		t.Fatal("first pull reopened a route a reply closed")
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
