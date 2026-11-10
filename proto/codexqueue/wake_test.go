package codexqueue

import (
	"errors"
	"testing"
	"time"
)

func newWakes() (*Wakes, *fakeQueue, *fakeClock) {
	c := newClock()
	q := newQueue()
	return NewWakes(q, c.Now, 30*time.Second), q, c
}

func states(w *Wakes, thread string) []GenState {
	var out []GenState
	for _, g := range w.Gens(thread) {
		out = append(out, g.State)
	}
	return out
}

func eq(a, b []GenState) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestWake_FirstSignal_ExpectQueuedOneRow(t *testing.T) {
	w, q, _ := newWakes()
	if _, err := w.Signal(th); err != nil {
		t.Fatal(err)
	}
	if !eq(states(w, th), []GenState{GenQueued}) || q.count(th) != 1 {
		t.Fatalf("%v rows %d", states(w, th), q.count(th))
	}
}

func TestWake_SecondSignalWhileQueued_ExpectNoSecondRow(t *testing.T) {
	w, q, _ := newWakes()
	w.Signal(th)
	w.Signal(th)
	if q.count(th) != 1 || len(w.Gens(th)) != 1 {
		t.Fatalf("rows %d gens %d", q.count(th), len(w.Gens(th)))
	}
}

func TestWake_MessageArrivesWhileAdmitted_ExpectNoRow(t *testing.T) {
	w, q, _ := newWakes()
	w.Signal(th)
	q.take(th)
	w.Observe(th)
	if !eq(states(w, th), []GenState{GenAdmitted}) {
		t.Fatalf("%v", states(w, th))
	}
	w.Signal(th)
	if q.count(th) != 0 || len(w.Gens(th)) != 1 {
		t.Fatalf("a signal in the admitted gap wrote a row: rows %d gens %d", q.count(th), len(w.Gens(th)))
	}
}

func TestWake_PullAfterAdmitted_ExpectCollectedClosedAndNextSignalOpensGen2(t *testing.T) {
	w, q, _ := newWakes()
	w.Signal(th)
	q.take(th)
	w.Observe(th)
	w.Pulled(th)
	if !eq(states(w, th), []GenState{GenCollected}) {
		t.Fatalf("%v", states(w, th))
	}
	w.Signal(th)
	if !eq(states(w, th), []GenState{GenCollected, GenQueued}) || q.count(th) != 1 {
		t.Fatalf("%v rows %d", states(w, th), q.count(th))
	}
}

func TestWake_PullWhileQueuedRowStillPresent_ExpectCollected(t *testing.T) {
	w, _, _ := newWakes()
	w.Signal(th)
	w.Pulled(th)
	if !eq(states(w, th), []GenState{GenCollected}) {
		t.Fatalf("%v", states(w, th))
	}
}

func TestWake_AdmittedNoPullBeforeBound_ExpectStillAdmitted(t *testing.T) {
	w, q, c := newWakes()
	w.Signal(th)
	q.take(th)
	w.Observe(th)
	c.Advance(29 * time.Second)
	w.Tick()
	if !eq(states(w, th), []GenState{GenAdmitted}) {
		t.Fatalf("%v", states(w, th))
	}
}

func TestWake_AdmittedNoPullAtBound_ExpectUnknownThenEachLaterArrivalCostsOneRow(t *testing.T) {
	w, q, c := newWakes()
	w.Signal(th)
	q.take(th)
	w.Observe(th)
	c.Advance(30 * time.Second)
	w.Tick()
	if !eq(states(w, th), []GenState{GenUnknown}) {
		t.Fatalf("%v", states(w, th))
	}
	w.Tick()
	w.Tick()
	if q.count(th) != 0 {
		t.Fatalf("a loop: ticks alone wrote %d rows", q.count(th))
	}
	w.Signal(th)
	if !eq(states(w, th), []GenState{GenUnknown, GenQueued}) || q.count(th) != 1 {
		t.Fatalf("%v rows %d", states(w, th), q.count(th))
	}
}

func TestWake_BusyTwiceThenCommits_ExpectQueuedWithinRetryBound(t *testing.T) {
	w, q, _ := newWakes()
	q.busyN = 2
	w.Signal(th)
	if !eq(states(w, th), []GenState{GenQueued}) {
		t.Fatalf("%v", states(w, th))
	}
}

func TestWake_BusyBeyondRetryBound_ExpectFailedNoRowAndNoLaterRetry(t *testing.T) {
	w, q, _ := newWakes()
	q.busyN = 100
	w.Signal(th)
	w.Tick()
	w.Replay()
	if !eq(states(w, th), []GenState{GenFailed}) || q.count(th) != 0 || q.inserts != 0 {
		t.Fatalf("%v rows %d", states(w, th), q.count(th))
	}
}

func TestWake_QueueAtCap_ExpectFailedNoRow(t *testing.T) {
	w, q, _ := newWakes()
	q.fill(th, 100)
	w.Signal(th)
	if !eq(states(w, th), []GenState{GenFailed}) || q.count(th) != 100 {
		t.Fatalf("%v rows %d", states(w, th), q.count(th))
	}
}

func TestWake_OurEarlierRowStillInTable_ExpectFailedNoSecondRow(t *testing.T) {
	w, q, c := newWakes()
	w.Signal(th)
	// the row was never taken (interrupted thread) and the generation went unknown by other means
	w.gens[th][0].State = GenUnknown
	_ = c
	w.Signal(th)
	if !eq(states(w, th), []GenState{GenUnknown, GenFailed}) || q.count(th) != 1 {
		t.Fatalf("%v rows %d", states(w, th), q.count(th))
	}
}

func TestWake_CrashAfterPendingReplayNoRow_ExpectUnknownNoRowWritten(t *testing.T) {
	w, q, _ := newWakes()
	w.Crash = func(p string) bool { return p == "after-pending" }
	if _, err := w.Signal(th); !errors.Is(err, ErrCrash) {
		t.Fatal(err)
	}
	w.Crash = nil
	w.Replay()
	if !eq(states(w, th), []GenState{GenUnknown}) || q.count(th) != 0 {
		t.Fatalf("%v rows %d", states(w, th), q.count(th))
	}
}

func TestWake_CrashAfterCommitReplayRowPresent_ExpectQueuedNoSecondRow(t *testing.T) {
	w, q, _ := newWakes()
	w.Crash = func(p string) bool { return p == "after-commit" }
	w.Signal(th)
	w.Crash = nil
	w.Replay()
	if !eq(states(w, th), []GenState{GenQueued}) || q.count(th) != 1 {
		t.Fatalf("%v rows %d", states(w, th), q.count(th))
	}
}

func TestWake_CrashAfterCommitCodexAlreadyTookRow_ExpectUnknownNoRowWritten(t *testing.T) {
	w, q, _ := newWakes()
	w.Crash = func(p string) bool { return p == "after-commit" }
	w.Signal(th)
	q.take(th)
	w.Crash = nil
	w.Replay()
	if !eq(states(w, th), []GenState{GenUnknown}) || q.count(th) != 0 {
		t.Fatalf("%v rows %d", states(w, th), q.count(th))
	}
}

func TestWake_ReplayQueuedRowStillPresent_ExpectStaysQueued(t *testing.T) {
	w, q, _ := newWakes()
	w.Signal(th)
	w.Replay()
	if !eq(states(w, th), []GenState{GenQueued}) || q.count(th) != 1 {
		t.Fatalf("%v", states(w, th))
	}
}

func TestWake_ReplayQueuedRowGone_ExpectAdmittedThenUnknownAtBound(t *testing.T) {
	w, q, c := newWakes()
	w.Signal(th)
	q.take(th)
	w.Replay()
	if !eq(states(w, th), []GenState{GenAdmitted}) {
		t.Fatalf("%v", states(w, th))
	}
	c.Advance(30 * time.Second)
	w.Tick()
	if !eq(states(w, th), []GenState{GenUnknown}) {
		t.Fatalf("%v", states(w, th))
	}
}

func TestWake_ReplayAlone_ExpectNoGenerationOpened(t *testing.T) {
	w, q, _ := newWakes()
	w.Replay()
	w.Replay()
	if len(w.Gens(th)) != 0 || q.count(th) != 0 {
		t.Fatal("replay opened a generation or wrote a row")
	}
}
