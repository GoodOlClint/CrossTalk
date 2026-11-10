package codexqueue

import (
	"errors"
	"testing"
	"time"
)

func newRouter() (*Router, *fakeQueue, *fakeClock) {
	c := newClock()
	q := newQueue()
	return &Router{MB: NewMailbox(c.Now, 60*time.Second), W: NewWakes(q, c.Now, 30*time.Second)}, q, c
}

func TestRouter_MailboxCommittedBeforeAnyWakeRow_ExpectMailboxedAtInsertTime(t *testing.T) {
	r, q, _ := newRouter()
	var at EntryState = -1
	q.onInsert = func(thread string) { at = r.MB.State(thread, "m1") }
	if err := r.Receive(th, "m1", true); err != nil {
		t.Fatal(err)
	}
	if at != Mailboxed {
		t.Fatalf("at wake-row insert the entry was %v, want mailboxed", at)
	}
}

func TestRouter_PullImmediatelyAfterRowCommit_ExpectEntryReturned(t *testing.T) {
	r, _, _ := newRouter()
	r.Receive(th, "m1", true)
	if res := r.Pull(th, "", 10); len(res.Entries) != 1 || res.Entries[0] != "m1" {
		t.Fatalf("%+v", res)
	}
}

func TestRouter_CrashBeforeSignal_ExpectPullableNoGenerationAndNextArrivalOpensOne(t *testing.T) {
	r, q, _ := newRouter()
	r.Crash = func(p string) bool { return p == "before-signal" }
	if err := r.Receive(th, "m1", true); !errors.Is(err, ErrCrash) {
		t.Fatal(err)
	}
	r.Crash = nil
	if len(r.W.Gens(th)) != 0 || q.count(th) != 0 || r.MB.State(th, "m1") != Mailboxed {
		t.Fatal("crash before signal left a generation, a row, or a lost entry")
	}
	r.Receive(th, "m2", true)
	if !eq(states(r.W, th), []GenState{GenQueued}) || q.count(th) != 1 {
		t.Fatalf("%v", states(r.W, th))
	}
	if res := r.Pull(th, "", 10); len(res.Entries) != 2 {
		t.Fatalf("one wake must drain both entries: %+v", res)
	}
}

func TestRouter_NoWakeRoute_ExpectMailboxedAndNoGeneration(t *testing.T) {
	r, q, _ := newRouter()
	r.Receive(th, "m1", false)
	if len(r.W.Gens(th)) != 0 || q.count(th) != 0 || r.MB.State(th, "m1") != Mailboxed {
		t.Fatal("row 4 route wrote a wake")
	}
}

func TestRouter_WakeWriteFails_ExpectEntryStaysMailboxedAndGenerationFailed(t *testing.T) {
	r, q, _ := newRouter()
	q.busyN = 100
	r.Receive(th, "m1", true)
	if !eq(states(r.W, th), []GenState{GenFailed}) || r.MB.State(th, "m1") != Mailboxed {
		t.Fatalf("%v %v", states(r.W, th), r.MB.State(th, "m1"))
	}
}

func TestRouter_PullClosesGeneration_ExpectCollected(t *testing.T) {
	r, q, _ := newRouter()
	r.Receive(th, "m1", true)
	q.take(th)
	r.W.Observe(th)
	r.Pull(th, "", 10)
	if !eq(states(r.W, th), []GenState{GenCollected}) {
		t.Fatalf("%v", states(r.W, th))
	}
}

// Codex review finding 2: an arrival during a paused pull must not lose its wake.
func pausedPull(t *testing.T, r *Router, during func()) {
	t.Helper()
	marked, proceed := make(chan struct{}), make(chan struct{})
	r.MB.AfterMark = func() { close(marked); <-proceed }
	done := make(chan PullResult)
	go func() { done <- r.Pull(th, "", 2) }()
	<-marked
	during()
	close(proceed)
	<-done
	r.MB.AfterMark = nil
}

func TestRouter_ArrivalDuringPausedPullWithMoreFalse_ExpectNewGenerationAndRow(t *testing.T) {
	r, q, _ := newRouter()
	want := 1
	r.Receive(th, "m1", true)
	q.take(th)
	pausedPull(t, r, func() { r.Receive(th, "m2", true) })
	if q.count(th) != want || !eq(states(r.W, th), []GenState{GenCollected, GenQueued}) {
		t.Fatalf("gens %v rows %d: m2 got no wake", states(r.W, th), q.count(th))
	}
	if res := r.Pull(th, "", 10); len(res.Entries) != 1 || res.Entries[0] != "m2" {
		t.Fatalf("%+v", res)
	}
}

func TestRouter_ArrivalDuringPausedPullWithMoreTrue_ExpectNoExtraWake(t *testing.T) {
	r, q, _ := newRouter()
	want := 0
	for _, id := range []string{"m1", "m2", "m3"} {
		r.Receive(th, id, true)
	}
	q.take(th)
	pausedPull(t, r, func() { r.Receive(th, "m4", true) })
	if q.count(th) != want || !eq(states(r.W, th), []GenState{GenCollected}) {
		t.Fatalf("gens %v rows %d: the model is still draining, no second wake", states(r.W, th), q.count(th))
	}
}

func TestRouter_ArrivalAfterPullResumes_ExpectNewGenerationAsBefore(t *testing.T) {
	r, q, _ := newRouter()
	want := 1
	r.Receive(th, "m1", true)
	q.take(th)
	pausedPull(t, r, func() {})
	r.Receive(th, "m2", true)
	if q.count(th) != want || !eq(states(r.W, th), []GenState{GenCollected, GenQueued}) {
		t.Fatalf("gens %v rows %d", states(r.W, th), q.count(th))
	}
}

// Verifier-found: a no-wake entry (route row 4) arriving during a paused pull must not get a wake.
func TestRouter_NoWakeArrivalDuringPausedPull_ExpectNoRow(t *testing.T) {
	r, q, _ := newRouter()
	r.Receive(th, "m1", true)
	q.take(th)
	pausedPull(t, r, func() { r.Receive(th, "m2", false) })
	if q.count(th) != 0 || !eq(states(r.W, th), []GenState{GenCollected}) {
		t.Fatalf("row-4 (no wake) entry got a wake: gens %v rows %d", states(r.W, th), q.count(th))
	}
	if res := r.Pull(th, "", 10); len(res.Entries) != 1 || res.Entries[0] != "m2" {
		t.Fatalf("the no-wake entry must stay pullable: %+v", res)
	}
}

func TestRouter_WakeEligibleAndNoWakeArrivalsDuringPausedPull_ExpectOneRow(t *testing.T) {
	r, q, _ := newRouter()
	r.Receive(th, "m1", true)
	q.take(th)
	pausedPull(t, r, func() { r.Receive(th, "m2", false); r.Receive(th, "m3", true) })
	if q.count(th) != 1 || len(r.W.Gens(th)) != 2 {
		t.Fatalf("gens %v rows %d", states(r.W, th), q.count(th))
	}
}

func TestRouter_TwoPausedPullsBothRecheck_ExpectOneRow(t *testing.T) {
	r, q, _ := newRouter()
	r.Receive(th, "m1", true)
	q.take(th)
	var n int
	ma, mb, go1 := make(chan struct{}), make(chan struct{}), make(chan struct{})
	r.MB.AfterMark = func() {
		n++
		if n == 1 {
			close(ma)
		} else {
			close(mb)
		}
		<-go1
	}
	da, db := make(chan PullResult), make(chan PullResult)
	go func() { da <- r.Pull(th, "", 10) }()
	<-ma
	go func() { db <- r.Pull(th, "", 10) }()
	<-mb
	r.Receive(th, "m2", true)
	close(go1)
	<-da
	<-db
	if q.count(th) != 1 {
		t.Fatalf("gens %v rows %d", states(r.W, th), q.count(th))
	}
}
