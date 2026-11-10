package receiver

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/msg"
)

func newMailboxEnv(t *testing.T) *env {
	e := newEnv(t)
	e.fake.SetDefaultInject(adapter.Outcome{State: adapter.Mailboxed})
	return e
}

func (e *env) mailboxed(text string) msg.Message {
	e.t.Helper()
	m := newMsg(msg.Inform, text)
	if r := e.recv(A2A, alice, m); r.State != adapter.Mailboxed {
		e.t.Fatalf("recv %+v", r)
	}
	return m
}

func (e *env) pull(after string, limit int) ([]Entry, bool) {
	e.t.Helper()
	es, more, err := e.r.Pull(after, limit)
	if err != nil {
		e.t.Fatal(err)
	}
	return es, more
}

func (e *env) dedupe(m msg.Message) string {
	st, _ := e.j.Dedupe(alice.AgentID, m.MessageID)
	return st
}

type signalAdapter struct {
	*adapter.Fake
	mu      sync.Mutex
	signals []string
	onCall  func(m msg.Message)
}

func (s *signalAdapter) MailSignal(_ context.Context, m msg.Message, _ adapter.Delivery) error {
	if s.onCall != nil {
		s.onCall(m)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.signals = append(s.signals, m.MessageID)
	return nil
}

func (s *signalAdapter) got() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.signals...)
}

func TestPull_MailboxedEntry_ExpectDeliveredAfterPull(t *testing.T) {
	e := newMailboxEnv(t)
	m := e.mailboxed("hi")
	if st := e.dedupe(m); st != "mailboxed" {
		t.Fatalf("before pull %q", st)
	}
	es, _ := e.pull("", 0)
	if len(es) != 1 || es[0].MessageID != m.MessageID || es[0].State != "delivered" || es[0].PossiblySeen {
		t.Fatalf("pull %+v", es)
	}
	if st := e.dedupe(m); st != "delivered" {
		t.Fatalf("after pull %q", st)
	}
}

func TestPull_SecondPullReturnsPossiblySeen_ExpectThirdPullEmpty(t *testing.T) {
	e := newMailboxEnv(t)
	m := e.mailboxed("hi")
	e.pull("", 0)
	es, _ := e.pull("", 0)
	if len(es) != 1 || es[0].MessageID != m.MessageID || !es[0].PossiblySeen || es[0].State != "delivered" || es[0].Message == nil {
		t.Fatalf("second pull %+v", es)
	}
	if st := e.dedupe(m); st != "confirmed" {
		t.Fatalf("after second pull %q", st)
	}
	if es, _ := e.pull("", 0); len(es) != 0 {
		t.Fatalf("third pull %+v", es)
	}
}

func TestConfirm_ReplyAfterFirstPull_ExpectConfirmedAndNotReturned(t *testing.T) {
	e := newMailboxEnv(t)
	m := e.mailboxed("hi")
	e.pull("", 0)
	if err := e.r.Confirm(m.MessageID); err != nil {
		t.Fatal(err)
	}
	if st := e.dedupe(m); st != "confirmed" {
		t.Fatalf("state %q", st)
	}
	if es, _ := e.pull("", 0); len(es) != 0 {
		t.Fatalf("pull after confirm %+v", es)
	}
}

func TestConfirm_LateReplyAfterReReturn_ExpectConfirmedNoRegression(t *testing.T) {
	e := newMailboxEnv(t)
	m := e.mailboxed("hi")
	e.pull("", 0)
	e.pull("", 0)
	if err := e.r.Confirm(m.MessageID); err != nil {
		t.Fatal(err)
	}
	if st := e.dedupe(m); st != "confirmed" {
		t.Fatalf("state %q", st)
	}
}

func TestConfirm_ReplyToMailboxedEntry_ExpectNotCollectedAndStillMailboxed(t *testing.T) {
	e := newMailboxEnv(t)
	m := e.mailboxed("hi")
	if err := e.r.Confirm(m.MessageID); !errors.Is(err, ErrNotCollected) {
		t.Fatalf("err %v", err)
	}
	if st := e.dedupe(m); st != "mailboxed" {
		t.Fatalf("state %q", st)
	}
}

func TestPull_AnotherThreadPullAndConfirm_ExpectEntryStillMailboxed(t *testing.T) {
	a, b := newMailboxEnv(t), newMailboxEnv(t)
	m := a.mailboxed("for a")
	if es, _ := b.pull("", 0); len(es) != 0 {
		t.Fatalf("other thread pulled %+v", es)
	}
	if err := b.r.Confirm(m.MessageID); err != nil {
		t.Fatal(err)
	}
	if st := a.dedupe(m); st != "mailboxed" {
		t.Fatalf("state %q", st)
	}
	if es, _ := a.pull("", 0); len(es) != 1 {
		t.Fatalf("owner pull %+v", es)
	}
}

func TestJournal_LateOutcomeAfterDeliveredOrConfirmed_ExpectStateNeverMovesBack(t *testing.T) {
	e := newMailboxEnv(t)
	m := e.mailboxed("hi")
	late := func(want string) {
		t.Helper()
		for _, st := range []string{"unknown", "accepted", "queued", "unreachable", "mailboxed", "submitted"} {
			if err := e.j.ResolveFor("", m.MessageID, st); err != nil {
				t.Fatal(err)
			}
			if err := e.j.SetDedupeState(alice.AgentID, m.MessageID, st); err != nil {
				t.Fatal(err)
			}
		}
		if got := e.dedupe(m); got != want {
			t.Fatalf("after late outcomes: %q, want %q", got, want)
		}
	}
	late("mailboxed")
	e.pull("", 0)
	late("delivered")
	if l, _ := e.j.DispatchState(m.MessageID); l.State != "mailboxed" {
		t.Fatalf("late outcome record landed: %+v", l)
	}
	e.pull("", 0)
	late("confirmed")
}

func TestPull_ConcurrentPullsOneThread_ExpectEveryEntryReturnedNoneLost(t *testing.T) {
	e := newMailboxEnv(t)
	const n = 12
	want := map[string]bool{}
	for i := 0; i < n; i++ {
		want[e.mailboxed("m").MessageID] = true
	}
	var mu sync.Mutex
	got := map[string]int{}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			es, _, err := e.r.Pull("", 0)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, en := range es {
				got[en.MessageID]++
			}
		}()
	}
	wg.Wait()
	for id := range want {
		if got[id] == 0 || got[id] > 2 {
			t.Errorf("entry %s returned %d times", id, got[id])
		}
	}
	if es, _ := e.pull("", 0); len(es) != 0 {
		t.Fatalf("leftover after four pulls %+v", es)
	}
}

func TestMailSignal_AfterMailboxedRecordCommits_ExpectCalledOncePerMailboxedMessage(t *testing.T) {
	e := newMailboxEnv(t)
	sa := &signalAdapter{Fake: e.fake}
	e.r = New(Config{Journal: e.j, Adapter: sa, Self: e.self})
	var seen string
	sa.onCall = func(m msg.Message) { seen = e.dedupe(m) }
	m := e.mailboxed("hi")
	if seen != "mailboxed" {
		t.Fatalf("dedupe state at signal time %q", seen)
	}
	if l, _ := e.j.DispatchState(m.MessageID); l.State != "mailboxed" {
		t.Fatalf("outcome at signal time %+v", l)
	}
	if got := sa.got(); len(got) != 1 || got[0] != m.MessageID {
		t.Fatalf("signals %v", got)
	}
	if r := e.recv(A2A, alice, m); !r.Duplicate || len(sa.got()) != 1 {
		t.Fatalf("duplicate signalled: %+v %v", r, sa.got())
	}
}

func TestMailSignal_DeliveredOrNonMailboxedStates_ExpectNeverCalled(t *testing.T) {
	e := newMailboxEnv(t)
	sa := &signalAdapter{Fake: e.fake}
	e.r = New(Config{Journal: e.j, Adapter: sa, Self: e.self})
	e.mailboxed("hi")
	e.pull("", 0)
	e.pull("", 0)
	e.fake.SetDefaultInject(adapter.Outcome{State: adapter.Accepted})
	e.recv(A2A, alice, newMsg(msg.Inform, "accepted"))
	e.fake.SetDefaultInject(adapter.Outcome{State: adapter.Unreachable})
	e.recv(A2A, alice, newMsg(msg.Inform, "unreachable"))
	if got := sa.got(); len(got) != 1 {
		t.Fatalf("signals %v, want only the mailboxed one", got)
	}
}

func TestReplay_ReopenedJournal_ExpectMailboxedDeliveredAndConfirmedPreserved(t *testing.T) {
	e := newMailboxEnv(t)
	confirmed := e.mailboxed("a")
	e.pull("", 0)
	if err := e.r.Confirm(confirmed.MessageID); err != nil {
		t.Fatal(err)
	}
	delivered := e.mailboxed("b")
	e.pull(confirmed.MessageID, 0)
	mailboxed := e.mailboxed("c")
	e.open(selfBinding, "inst-2")
	for _, c := range []struct {
		m    msg.Message
		want string
	}{{confirmed, "confirmed"}, {delivered, "delivered"}, {mailboxed, "mailboxed"}} {
		if got := e.dedupe(c.m); got != c.want {
			t.Fatalf("%s after reopen: %q, want %q", c.want, got, c.want)
		}
	}
	es, _ := e.pull("", 0)
	if len(es) != 2 || es[0].MessageID != delivered.MessageID || !es[0].PossiblySeen || es[1].MessageID != mailboxed.MessageID || es[1].PossiblySeen {
		t.Fatalf("pull after reopen %+v", es)
	}
}

func TestPull_PageLimitLeavesMailboxed_ExpectMoreFlag(t *testing.T) {
	e := newMailboxEnv(t)
	e.mailboxed("a")
	e.mailboxed("b")
	e.mailboxed("c")
	es, more := e.pull("", 2)
	if len(es) != 2 || !more {
		t.Fatalf("page %+v more %v", es, more)
	}
	es, more = e.pull("", 0)
	if len(es) != 3 || more || !es[0].PossiblySeen || !es[1].PossiblySeen || es[2].PossiblySeen {
		t.Fatalf("second pull %+v more %v", es, more)
	}
}

func TestPull_DeliveredEntryAloneBehindLimit_ExpectNoMoreFlag(t *testing.T) {
	e := newMailboxEnv(t)
	first := e.mailboxed("a")
	e.mailboxed("b")
	e.pull(first.MessageID, 0)
	es, more := e.pull("", 1)
	if len(es) != 1 || es[0].MessageID != first.MessageID || more {
		t.Fatalf("page %+v more %v: a delivered entry left behind must not set the drain flag", es, more)
	}
}
