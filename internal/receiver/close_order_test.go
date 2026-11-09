package receiver

import (
	"slices"
	"testing"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/msg"
)

type appendLog struct {
	recs []journal.Record
	fail func(journal.Record) bool
}

func (l *appendLog) before(r journal.Record) error {
	if l.fail != nil && l.fail(r) {
		return journal.ErrClosed
	}
	l.recs = append(l.recs, r)
	return nil
}

func (e *env) openLogged(l *appendLog) {
	j, _, err := journal.Open(e.dir, selfID, jbind(selfBinding, "inst-1"), journal.Options{BeforeAppend: l.before})
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { j.Close() })
	e.j = j
	e.r = New(Config{Journal: j, Adapter: e.fake, Self: e.self})
}

func isRouteClosed(r journal.Record) bool {
	return r.Type == journal.TypeRoute && r.State == journal.StateClosed
}

func isUnreachableOutcome(r journal.Record) bool {
	return (r.Type == journal.TypeOutcome || r.Type == journal.TypeDedupe) && r.State == string(adapter.Unreachable)
}

func TestRouteClosedBeforeUnreachableRecorded(t *testing.T) {
	e := newEnv(t)
	l := &appendLog{}
	e.openLogged(l)
	e.fake.ScriptInject(adapter.Outcome{State: adapter.Unreachable, NoWrite: true})
	if res := e.recv(A2A, alice, newMsg(msg.Inform, "x")); res.State != adapter.Unreachable {
		t.Fatalf("%+v", res)
	}
	closed := slices.IndexFunc(l.recs, isRouteClosed)
	first := slices.IndexFunc(l.recs, isUnreachableOutcome)
	if closed < 0 || first < 0 || closed > first {
		t.Fatalf("route closed at %d, unreachable recorded at %d", closed, first)
	}
}

func TestFailedRouteCloseIsUnknownNotUnreachable(t *testing.T) {
	e := newEnv(t)
	l := &appendLog{fail: isRouteClosed}
	e.openLogged(l)
	e.fake.ScriptInject(adapter.Outcome{State: adapter.Unreachable, NoWrite: true})
	m := newMsg(msg.Inform, "x")
	res := e.recv(A2A, alice, m)
	if res.State != adapter.Unknown || res.Reason != ReasonJournal {
		t.Fatalf("%+v", res)
	}
	if i := slices.IndexFunc(l.recs, isUnreachableOutcome); i >= 0 {
		t.Fatalf("unreachable recorded despite failed route close: %+v", l.recs[i])
	}
	if st, _ := e.j.Dedupe(alice.key(), m.MessageID); st == string(adapter.Unreachable) {
		t.Fatal("dedupe shows unreachable")
	}
}
