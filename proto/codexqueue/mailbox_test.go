package codexqueue

import (
	"errors"
	"sync"
	"testing"
	"time"
)

const th, other = "thread-A", "thread-B"

func newMB() (*Mailbox, *fakeClock) {
	c := newClock()
	return NewMailbox(c.Now, 60*time.Second), c
}

func TestMailbox_PullMarksCollectingAndIssuesToken_ExpectCollecting(t *testing.T) {
	m, _ := newMB()
	m.Commit(th, "m1")
	r := m.Pull(th, "", 10)
	if len(r.Entries) != 1 || r.Token == "" || m.State(th, "m1") != Collecting {
		t.Fatalf("got %+v state %v", r, m.State(th, "m1"))
	}
}

func TestMailbox_MonotonicResolve_ExpectNoBackwardOrSkippingMoves(t *testing.T) {
	m, _ := newMB()
	m.Commit(th, "m1")
	m.Resolve(th, "m1", Accepted)
	if m.State(th, "m1") != Mailboxed {
		t.Fatalf("mailboxed -> accepted without a pull must be ignored, got %v", m.State(th, "m1"))
	}
	m.Pull(th, "", 10)
	m.Resolve(th, "m1", Mailboxed)
	if m.State(th, "m1") != Collecting {
		t.Fatalf("late mailboxed outcome overwrote collecting: %v", m.State(th, "m1"))
	}
	m.Resolve(th, "m1", Accepted)
	m.Resolve(th, "m1", Collecting)
	m.Resolve(th, "m1", Unknown)
	if m.State(th, "m1") != Accepted {
		t.Fatalf("accepted must be terminal, got %v", m.State(th, "m1"))
	}
}

func TestMailbox_ReplySettlesCollected_ExpectAccepted(t *testing.T) {
	m, _ := newMB()
	m.Commit(th, "m1")
	m.Pull(th, "", 10)
	if err := m.Reply(th, "m1"); err != nil || m.State(th, "m1") != Accepted {
		t.Fatalf("err %v state %v", err, m.State(th, "m1"))
	}
}

func TestMailbox_ReplyToMailboxedEntry_ExpectNotCollectedAndStillMailboxed(t *testing.T) {
	m, _ := newMB()
	m.Commit(th, "m1")
	if err := m.Reply(th, "m1"); !errors.Is(err, ErrNotCollected) || m.State(th, "m1") != Mailboxed {
		t.Fatalf("err %v state %v", err, m.State(th, "m1"))
	}
}

func TestMailbox_CoveringCursorOnNextPull_ExpectAccepted(t *testing.T) {
	m, _ := newMB()
	m.Commit(th, "m1")
	r := m.Pull(th, "", 10)
	m.Pull(th, r.Next, 10)
	if m.State(th, "m1") != Accepted {
		t.Fatalf("state %v", m.State(th, "m1"))
	}
}

func TestMailbox_SecondPullWithoutCursor_ExpectStillCollecting(t *testing.T) {
	m, _ := newMB()
	m.Commit(th, "m1")
	m.Pull(th, "", 10)
	m.Pull(th, "", 10)
	if m.State(th, "m1") != Collecting {
		t.Fatalf("state %v", m.State(th, "m1"))
	}
}

func TestMailbox_UnissuedCursor_ExpectNoSettle(t *testing.T) {
	m, _ := newMB()
	m.Commit(th, "m1")
	m.Pull(th, "", 10)
	m.Pull(th, "m1", 10)
	if m.State(th, "m1") != Accepted {
		t.Fatalf("issued cursor must settle, got %v", m.State(th, "m1"))
	}
	m.Commit(th, "m2")
	m.Commit(th, "m3")
	m.Pull(th, "", 1)
	m.Pull(th, "m99", 10)
	if m.State(th, "m2") != Collecting {
		t.Fatalf("a cursor never issued by a pull settled an entry: %v", m.State(th, "m2"))
	}
}

func TestMailbox_EchoedTokenSettles_ExpectAccepted(t *testing.T) {
	m, _ := newMB()
	m.Commit(th, "m1")
	r := m.Pull(th, "", 10)
	m.Ack(th, "wrong")
	if m.State(th, "m1") != Collecting {
		t.Fatalf("wrong token settled: %v", m.State(th, "m1"))
	}
	m.Ack(th, r.Token)
	if m.State(th, "m1") != Accepted {
		t.Fatalf("state %v", m.State(th, "m1"))
	}
}

func TestMailbox_CallsFromAnotherThread_ExpectNeverSettle(t *testing.T) {
	m, _ := newMB()
	m.Commit(th, "m1")
	r := m.Pull(th, "", 10)
	if err := m.Reply(other, "m1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reply from other thread: %v", err)
	}
	m.Ack(other, r.Token)
	m.Pull(other, r.Next, 10)
	if m.State(th, "m1") != Collecting {
		t.Fatalf("another thread settled this thread's entry: %v", m.State(th, "m1"))
	}
}

func TestMailbox_NoEvidenceBeforeBound_ExpectStillCollecting(t *testing.T) {
	m, c := newMB()
	m.Commit(th, "m1")
	m.Pull(th, "", 10)
	c.Advance(59 * time.Second)
	m.Expire()
	if m.State(th, "m1") != Collecting {
		t.Fatalf("state %v", m.State(th, "m1"))
	}
}

func TestMailbox_NoEvidenceAtBound_ExpectUnknownAndNotRepulled(t *testing.T) {
	m, c := newMB()
	m.Commit(th, "m1")
	m.Pull(th, "", 10)
	c.Advance(60 * time.Second)
	m.Expire()
	if m.State(th, "m1") != Unknown {
		t.Fatalf("state %v", m.State(th, "m1"))
	}
	if r := m.Pull(th, "", 10); len(r.Entries) != 0 {
		t.Fatalf("unknown entry returned by a pull: %+v", r)
	}
}

func TestMailbox_LateReplyAfterUnknown_ExpectStaysUnknown(t *testing.T) {
	m, c := newMB()
	m.Commit(th, "m1")
	m.Pull(th, "", 10)
	c.Advance(time.Minute)
	m.Expire()
	if err := m.Reply(th, "m1"); err != nil || m.State(th, "m1") != Unknown {
		t.Fatalf("err %v state %v", err, m.State(th, "m1"))
	}
}

func TestMailbox_ReplayAfterCrashWithCollecting_ExpectUnknown(t *testing.T) {
	m, _ := newMB()
	m.Commit(th, "m1")
	m.Commit(th, "m2")
	m.Pull(th, "", 1)
	m.ReplayAfterCrash()
	if m.State(th, "m1") != Unknown || m.State(th, "m2") != Mailboxed {
		t.Fatalf("m1 %v m2 %v", m.State(th, "m1"), m.State(th, "m2"))
	}
}

func TestMailbox_PageLimit_ExpectMoreFlagAndSecondPullSettlesFirstPage(t *testing.T) {
	m, _ := newMB()
	for _, id := range []string{"m1", "m2", "m3"} {
		m.Commit(th, id)
	}
	r1 := m.Pull(th, "", 2)
	if len(r1.Entries) != 2 || !r1.More {
		t.Fatalf("%+v", r1)
	}
	r2 := m.Pull(th, r1.Next, 2)
	if len(r2.Entries) != 1 || r2.More || r2.Entries[0] != "m3" {
		t.Fatalf("%+v", r2)
	}
	if m.State(th, "m1") != Accepted || m.State(th, "m2") != Accepted || m.State(th, "m3") != Collecting {
		t.Fatalf("states %v %v %v", m.State(th, "m1"), m.State(th, "m2"), m.State(th, "m3"))
	}
}

func TestMailbox_ConcurrentPullAndListWithResponseWithheld_ExpectCollectingThenUnknown(t *testing.T) {
	m, c := newMB()
	m.Commit(th, "m1")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); m.Other(th) }()
	}
	var res PullResult
	wg.Add(1)
	go func() { defer wg.Done(); res = m.Pull(th, "", 10) }()
	wg.Wait()
	_ = res // the response never reaches the model in this scenario
	if m.State(th, "m1") != Collecting {
		t.Fatalf("a concurrent list_agents settled the entry: %v", m.State(th, "m1"))
	}
	c.Advance(time.Minute)
	m.Expire()
	if m.State(th, "m1") != Unknown {
		t.Fatalf("state %v", m.State(th, "m1"))
	}
}
