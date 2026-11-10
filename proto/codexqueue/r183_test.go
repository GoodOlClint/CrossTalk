package codexqueue

import "testing"

// R183 (a): a prompt the user queues in the Codex TUI is not in the table and is folded
// into the wake turn as a steer. Accepted, not a reject condition.
func TestR183a_TUIQueuedPromptFoldedIntoWakeTurn_ExpectNotInTableAndWakeFlowsNormally(t *testing.T) {
	r, q, _ := newRouter()
	sim := newSim(q, th)
	sim.typeTUIQueued("operatorq")
	if q.count(th) != 0 {
		t.Fatal("the TUI follow-up queue must not be in the table")
	}
	r.Receive(th, "m1", true)
	if !sim.dispatch() {
		t.Fatal("watcher did not dispatch the wake")
	}
	if len(sim.turns) != 1 || len(sim.turns[0].inputs) != 2 || sim.turns[0].inputs[1] != "operatorq" || sim.turns[0].trigger != "queue" {
		t.Fatalf("turns %+v: want one queue turn carrying the wake and the operator prompt", sim.turns)
	}
	if len(sim.tuiQueue) != 0 {
		t.Fatal("the folded prompt must not also run as its own turn")
	}
	r.W.Observe(th)
	res := r.Pull(th, "", 10)
	if len(res.Entries) != 1 || !eq(states(r.W, th), []GenState{GenCollected}) {
		t.Fatalf("pull %+v gens %v: a folded prompt must not disturb mailbox or generation", res, states(r.W, th))
	}
}

// R183 (b): kill -9 at the moment of consumption loses the wake; mail stays pullable.
func TestR183b_KillAtConsumption_ExpectWakeLostMailPullableUnknownAtBoundNoReplay(t *testing.T) {
	r, q, c := newRouter()
	sim := newSim(q, th)
	r.Receive(th, "m1", true)
	if !sim.killAtConsumption() {
		t.Fatal("no row to consume")
	}
	sim.resume()
	if sim.dispatch() || len(sim.turns) != 0 {
		t.Fatal("a consumed row was redelivered after the crash")
	}
	r.W.Observe(th)
	if !eq(states(r.W, th), []GenState{GenAdmitted}) {
		t.Fatalf("%v", states(r.W, th))
	}
	c.Advance(r.W.bound)
	r.W.Tick()
	if !eq(states(r.W, th), []GenState{GenUnknown}) || q.count(th) != 0 {
		t.Fatalf("%v rows %d: no replay and no new row without a new arrival", states(r.W, th), q.count(th))
	}
	if res := r.Pull(th, "", 10); len(res.Entries) != 1 || res.Entries[0] != "m1" {
		t.Fatalf("mail lost with the wake: %+v", res)
	}
	r.Receive(th, "m2", true)
	if q.count(th) != 1 || len(r.W.Gens(th)) != 2 {
		t.Fatalf("next arrival must open generation 2: rows %d gens %d", q.count(th), len(r.W.Gens(th)))
	}
}
