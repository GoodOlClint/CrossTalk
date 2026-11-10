package codex

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/msg"
	"github.com/GoodOlClint/crosstalk/internal/ownerproof"
)

const wThread = "019a0000-0000-7000-8000-00000000aaaa"

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type wakeEnv struct {
	t     *testing.T
	queue string
	clk   *clock
	gate  func(*QueueGate)
}

func newWakeEnv(t *testing.T) *wakeEnv {
	return &wakeEnv{t: t, queue: newQueueFixture(t, t.TempDir()), clk: &clock{t: t0}}
}

// wakes is one process's view: a fresh Wakes over dir (a restart is a new
// value over the same dir).
func (e *wakeEnv) wakes(dir string) *Wakes {
	return &Wakes{Dir: filepath.Join(dir, "codex-wakes"), Now: e.clk.now, Bound: time.Minute, Gate: func() *QueueGate {
		g := testGate(e.queue)
		if e.gate != nil {
			e.gate(g)
		}
		return g
	}}
}

func ok() error { return nil }

func (e *wakeEnv) signal(w *Wakes, id string) error {
	return w.Signal(context.Background(), wThread, id, true, ok)
}

type qrow struct {
	id, thread, payload string
	order               int
}

func (e *wakeEnv) rows() []qrow {
	e.t.Helper()
	db := openFixture(e.t, e.queue)
	rs, err := db.Query("SELECT id, thread_id, payload_json, queue_order FROM queued_items ORDER BY thread_id, queue_order")
	if err != nil {
		e.t.Fatal(err)
	}
	defer rs.Close()
	var out []qrow
	for rs.Next() {
		var r qrow
		if err := rs.Scan(&r.id, &r.thread, &r.payload, &r.order); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func (e *wakeEnv) wakeRows() []qrow {
	var out []qrow
	for _, r := range e.rows() {
		if strings.Contains(r.payload, WakeText) {
			out = append(out, r)
		}
	}
	return out
}

// take is Codex dispatching every row of thread.
func (e *wakeEnv) take() {
	execFixture(e.t, e.queue, "DELETE FROM queued_items WHERE thread_id = '"+wThread+"'")
}

func (e *wakeEnv) userRow(thread string, order int) {
	execFixture(e.t, e.queue, fmt.Sprintf("INSERT INTO queued_items(id, thread_id, payload_json, queue_order, created_at_ms, updated_at_ms) VALUES ('u-%s-%d', '%s', '{}', %d, 0, 0)", thread, order, thread, order))
}

func gen(t *testing.T, w *Wakes) Generation {
	t.Helper()
	g, err := w.Generation(wThread)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func wantState(t *testing.T, w *Wakes, n int, s GenState) Generation {
	t.Helper()
	g := gen(t, w)
	if g.N != n || g.State != s {
		t.Fatalf("generation %d %s (%s), want %d %s", g.N, g.State, g.Reason, n, s)
	}
	return g
}

func TestWakes_FirstEligibleSignal_ExpectOneQueuedRowWithFixedPayload(t *testing.T) {
	e := newWakeEnv(t)
	e.userRow(wThread, 0)
	e.userRow(wThread, 1)
	w := e.wakes(t.TempDir())
	if err := e.signal(w, "msg-secret-id"); err != nil {
		t.Fatal(err)
	}
	wantState(t, w, 1, GenQueued)
	rs := e.wakeRows()
	if len(rs) != 1 || rs[0].thread != wThread || rs[0].order != 2 {
		t.Fatalf("wake rows %+v, want one at queue_order 2", rs)
	}
	want := `{"UserInput":{"content":[{"type":"text","text":"` + WakeText + `","text_elements":[]}],"client_id":"` + wakeID(wThread, 1) + `"}}`
	if rs[0].payload != want {
		t.Fatalf("payload %s\nwant    %s", rs[0].payload, want)
	}
	if strings.Contains(rs[0].payload, "msg-secret-id") {
		t.Fatal("payload carries the message id")
	}
}

func TestWakes_EmptyQueue_ExpectQueueOrderZero(t *testing.T) {
	e := newWakeEnv(t)
	e.userRow("019a0000-0000-7000-8000-00000000bbbb", 7)
	if err := e.signal(e.wakes(t.TempDir()), "m1"); err != nil {
		t.Fatal(err)
	}
	if rs := e.wakeRows(); len(rs) != 1 || rs[0].order != 0 {
		t.Fatalf("rows %+v", rs)
	}
}

func TestWakes_NotEligible_ExpectNoGenerationNoRow(t *testing.T) {
	e := newWakeEnv(t)
	w := e.wakes(t.TempDir())
	if err := w.Signal(context.Background(), wThread, "m1", false, ok); err != nil {
		t.Fatal(err)
	}
	if g := gen(t, w); g.N != 0 || g.State != "" || len(g.Eligible) != 0 {
		t.Fatalf("generation %+v", g)
	}
	if len(e.rows()) != 0 {
		t.Fatal("row written")
	}
}

func TestWakes_ArrivalWhileQueuedOrAdmitted_ExpectNoSecondRow(t *testing.T) {
	e := newWakeEnv(t)
	w := e.wakes(t.TempDir())
	e.signal(w, "m1")
	e.signal(w, "m2")
	wantState(t, w, 1, GenQueued)
	if n := len(e.wakeRows()); n != 1 {
		t.Fatalf("rows %d while queued", n)
	}
	e.take()
	e.signal(w, "m3")
	wantState(t, w, 1, GenAdmitted)
	if n := len(e.wakeRows()); n != 0 {
		t.Fatalf("rows %d while admitted", n)
	}
	e.clk.add(59 * time.Second)
	e.signal(w, "m4")
	wantState(t, w, 1, GenAdmitted)
	if n := len(e.wakeRows()); n != 0 {
		t.Fatalf("rows %d inside the bound", n)
	}
}

func TestWakes_AdmittedPastBound_ExpectUnknownThenNextArrivalOpensNext(t *testing.T) {
	e := newWakeEnv(t)
	w := e.wakes(t.TempDir())
	e.signal(w, "m1")
	e.take()
	w.Tick(context.Background(), nil)
	wantState(t, w, 1, GenAdmitted)
	e.clk.add(time.Minute)
	w.Tick(context.Background(), nil)
	wantState(t, w, 1, GenUnknown)
	if n := len(e.wakeRows()); n != 0 {
		t.Fatalf("tick wrote %d rows", n)
	}
	e.signal(w, "m2")
	wantState(t, w, 2, GenQueued)
	if rs := e.wakeRows(); len(rs) != 1 || !strings.Contains(rs[0].payload, wakeID(wThread, 2)) {
		t.Fatalf("rows %+v, want generation 2's", rs)
	}
}

func TestWakes_PullClosesGeneration_ExpectCollectedThenNextArrivalOpensNext(t *testing.T) {
	e := newWakeEnv(t)
	w := e.wakes(t.TempDir())
	e.signal(w, "m1")
	e.take()
	if err := w.Pulled(context.Background(), wThread, false, func() ([]string, error) { return nil, nil }, ok); err != nil {
		t.Fatal(err)
	}
	wantState(t, w, 1, GenCollected)
	e.signal(w, "m2")
	wantState(t, w, 2, GenQueued)
}

func TestWakes_PauseAfterCommitThenPull_ExpectCollected(t *testing.T) {
	e := newWakeEnv(t)
	w := e.wakes(t.TempDir())
	e.signal(w, "m1")
	if err := w.Pulled(context.Background(), wThread, false, func() ([]string, error) { return nil, nil }, ok); err != nil {
		t.Fatal(err)
	}
	wantState(t, w, 1, GenCollected)
}

func TestWakes_Recheck_ExpectNextGenerationOnlyForWakeEligibleMailboxed(t *testing.T) {
	cases := []struct {
		name      string
		more      bool
		mailboxed []string
		wantGen   int
	}{
		{"eligible arrival after snapshot", false, []string{"m2"}, 2},
		{"every eligible entry delivered", false, nil, 1},
		{"only a mailbox-only entry left", false, []string{"m-nextturn"}, 1},
		{"more left to drain", true, []string{"m2"}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newWakeEnv(t)
			w := e.wakes(t.TempDir())
			e.signal(w, "m1")
			e.signal(w, "m2")
			w.Signal(context.Background(), wThread, "m-nextturn", false, ok)
			e.take()
			if err := w.Pulled(context.Background(), wThread, c.more, func() ([]string, error) { return c.mailboxed, nil }, ok); err != nil {
				t.Fatal(err)
			}
			g := gen(t, w)
			if g.N != c.wantGen {
				t.Fatalf("generation %d %s, want %d", g.N, g.State, c.wantGen)
			}
			wantRows := 0
			if c.wantGen == 2 {
				wantRows = 1
			}
			if n := len(e.wakeRows()); n != wantRows {
				t.Fatalf("rows %d, want %d", n, wantRows)
			}
		})
	}
}

func TestWakes_PullWithNoOpenGeneration_ExpectNoRecheck(t *testing.T) {
	e := newWakeEnv(t)
	w := e.wakes(t.TempDir())
	e.signal(w, "m1")
	e.take()
	w.Tick(context.Background(), nil)
	e.clk.add(time.Minute)
	w.Tick(context.Background(), nil)
	w.Pulled(context.Background(), wThread, false, func() ([]string, error) { return []string{"m1"}, nil }, ok)
	if len(e.wakeRows()) != 0 {
		t.Fatal("a pull that closed nothing re-checked")
	}
	wantState(t, w, 1, GenUnknown)
}

func TestWakes_CrashAfterPending_ExpectReplayUnknownAndNoRow(t *testing.T) {
	e := newWakeEnv(t)
	dir := t.TempDir()
	w := e.wakes(dir)
	w.crash = func(p string) error {
		if p == "after-pending" {
			return errors.New("crash")
		}
		return nil
	}
	e.signal(w, "m1")
	wantState(t, w, 1, GenPending)
	r := e.wakes(dir)
	if _, err := r.load(context.Background(), wThread); err != nil {
		t.Fatal(err)
	}
	wantState(t, r, 1, GenUnknown)
	if len(e.rows()) != 0 {
		t.Fatal("replay wrote a row")
	}
}

func TestWakes_CrashAfterCommit_ExpectReplayQueuedOrUnknownNeverSecondRow(t *testing.T) {
	for _, taken := range []bool{false, true} {
		t.Run(fmt.Sprint("taken=", taken), func(t *testing.T) {
			e := newWakeEnv(t)
			dir := t.TempDir()
			w := e.wakes(dir)
			w.crash = func(p string) error {
				if p == "after-commit" {
					return errors.New("crash")
				}
				return nil
			}
			e.signal(w, "m1")
			wantState(t, w, 1, GenPending)
			want, rows := GenQueued, 1
			if taken {
				e.take()
				want, rows = GenUnknown, 0
			}
			r := e.wakes(dir)
			if _, err := r.load(context.Background(), wThread); err != nil {
				t.Fatal(err)
			}
			wantState(t, r, 1, want)
			if n := len(e.wakeRows()); n != rows {
				t.Fatalf("rows %d after replay, want %d", n, rows)
			}
			e.signal(r, "m2")
			wantGen := 1
			if taken {
				wantGen = 2
			}
			if g := gen(t, r); g.N != wantGen || len(e.wakeRows()) != 1 {
				t.Fatalf("after the next arrival: generation %d, rows %d", g.N, len(e.wakeRows()))
			}
		})
	}
}

func TestWakes_ReplayQueued_ExpectQueuedWithRowAdmittedWithout(t *testing.T) {
	for _, taken := range []bool{false, true} {
		t.Run(fmt.Sprint("taken=", taken), func(t *testing.T) {
			e := newWakeEnv(t)
			dir := t.TempDir()
			e.signal(e.wakes(dir), "m1")
			want := GenQueued
			if taken {
				e.take()
				want = GenAdmitted
			}
			before := len(e.rows())
			r := e.wakes(dir)
			if _, err := r.load(context.Background(), wThread); err != nil {
				t.Fatal(err)
			}
			wantState(t, r, 1, want)
			if len(e.rows()) != before {
				t.Fatal("replay changed the queue")
			}
		})
	}
}

func TestWakes_ReplayPendingQueueUnreadable_ExpectUnknown(t *testing.T) {
	e := newWakeEnv(t)
	dir := t.TempDir()
	w := e.wakes(dir)
	w.crash = func(p string) error {
		if p == "after-commit" {
			return errors.New("crash")
		}
		return nil
	}
	e.signal(w, "m1")
	e.gate = func(g *QueueGate) {
		g.Version = func(context.Context, ownerproof.Proc) (string, error) { return "0.168.0", nil }
	}
	r := e.wakes(dir)
	r.load(context.Background(), wThread)
	wantState(t, r, 1, GenUnknown)
}

func TestWakes_GateFailures_ExpectFailedAndNoRow(t *testing.T) {
	cases := map[string]func(e *wakeEnv){
		"untested version": func(e *wakeEnv) {
			e.gate = func(g *QueueGate) {
				g.Version = func(context.Context, ownerproof.Proc) (string, error) { return "0.168.0", nil }
			}
		},
		"missing trigger": func(e *wakeEnv) { execFixture(e.t, e.queue, "DROP TRIGGER queued_items_revision_after_insert") },
		"extra migration": func(e *wakeEnv) {
			execFixture(e.t, e.queue, "INSERT INTO _sqlx_migrations(version, description, success, checksum, execution_time) VALUES (3, 'x', 1, x'00', 0)")
		},
		"missing file": func(e *wakeEnv) {
			for _, s := range []string{"", "-wal", "-shm"} {
				os.Remove(e.queue + s)
			}
		},
		"replaced inside the transaction": func(e *wakeEnv) {
			other := newQueueFixture(e.t, e.t.TempDir())
			e.gate = func(g *QueueGate) { g.afterBegin = func() { replaceFile(e.t, other, g.Path) } }
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			e := newWakeEnv(t)
			setup(e)
			w := e.wakes(t.TempDir())
			e.signal(w, "m1")
			g := wantState(t, w, 1, GenFailed)
			if !strings.Contains(g.Reason, "gate failed") {
				t.Fatalf("reason %q", g.Reason)
			}
			if _, err := os.Stat(e.queue); err == nil && len(e.rows()) != 0 {
				t.Fatal("row written")
			}
		})
	}
}

func TestWakes_QueueAtCap_ExpectFailedAndNoRow(t *testing.T) {
	e := newWakeEnv(t)
	for i := range MaxQueueItems {
		e.userRow("019a0000-0000-7000-8000-00000000bbbb", i)
	}
	w := e.wakes(t.TempDir())
	e.signal(w, "m1")
	g := wantState(t, w, 1, GenFailed)
	if !strings.Contains(g.Reason, ErrQueueFull.Error()) || len(e.wakeRows()) != 0 {
		t.Fatalf("reason %q rows %d", g.Reason, len(e.wakeRows()))
	}
}

func TestWakes_BusyPastTimeout_ExpectFailedNoRetryUntilNextArrival(t *testing.T) {
	e := newWakeEnv(t)
	db := openFixture(t, e.queue)
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	conn.ExecContext(context.Background(), "BEGIN IMMEDIATE")
	w := e.wakes(t.TempDir())
	e.signal(w, "m1")
	g := wantState(t, w, 1, GenFailed)
	if !strings.Contains(g.Reason, "busy") {
		t.Fatalf("reason %q", g.Reason)
	}
	conn.ExecContext(context.Background(), "ROLLBACK")
	conn.Close()
	w.Tick(context.Background(), nil)
	w.Pulled(context.Background(), wThread, false, func() ([]string, error) { return []string{"m1"}, nil }, ok)
	if n := len(e.wakeRows()); n != 0 {
		t.Fatalf("a failed generation was retried: %d rows", n)
	}
	e.signal(w, "m2")
	wantState(t, w, 2, GenQueued)
}

func TestWakes_RowFromAnotherWriter_ExpectFailedNoSecondRow(t *testing.T) {
	e := newWakeEnv(t)
	e.signal(e.wakes(t.TempDir()), "m1")
	w := e.wakes(t.TempDir())
	e.signal(w, "m1")
	g := wantState(t, w, 1, GenFailed)
	if !strings.Contains(g.Reason, ErrWakeRowExists.Error()) || len(e.wakeRows()) != 1 {
		t.Fatalf("reason %q rows %d", g.Reason, len(e.wakeRows()))
	}
}

func TestWakes_RepeatedFailures_ExpectOneCoalescedRecord(t *testing.T) {
	e := newWakeEnv(t)
	e.gate = func(g *QueueGate) {
		g.Version = func(context.Context, ownerproof.Proc) (string, error) { return "0.168.0", nil }
	}
	dir := t.TempDir()
	w := e.wakes(dir)
	for i := range 200 {
		e.signal(w, fmt.Sprint("m", i%3))
	}
	g := wantState(t, w, 200, GenFailed)
	if g.Failures != 200 || len(g.Eligible) != 3 {
		t.Fatalf("failures %d eligible %v", g.Failures, g.Eligible)
	}
	recs, _ := filepath.Glob(filepath.Join(w.Dir, "*.json"))
	if len(recs) != 1 {
		t.Fatalf("%d records in the wake dir", len(recs))
	}
	if fi, _ := os.Stat(w.path(wThread)); fi.Size() > 2048 {
		t.Fatalf("record grew to %d bytes", fi.Size())
	}
	e.gate = nil
	e.signal(w, "m9")
	if g := wantState(t, w, 201, GenQueued); g.Failures != 0 {
		t.Fatalf("failures %d after a success", g.Failures)
	}
}

func TestWakes_PrecheckFails_ExpectNoGenerationNoRow(t *testing.T) {
	e := newWakeEnv(t)
	w := e.wakes(t.TempDir())
	err := w.Signal(context.Background(), wThread, "m1", true, func() error { return ErrHosted })
	if !errors.Is(err, ErrHosted) {
		t.Fatalf("err %v", err)
	}
	if g := gen(t, w); g.N != 0 || g.State != "" || len(e.rows()) != 0 {
		t.Fatalf("generation %+v rows %d", g, len(e.rows()))
	}
}

func TestWakes_ConcurrentWriters_ExpectOneRowPerThread(t *testing.T) {
	e := newWakeEnv(t)
	e.gate = func(g *QueueGate) { g.BusyTimeout = 5 * time.Second }
	threads := []string{wThread, "019a0000-0000-7000-8000-00000000bbbb", "019a0000-0000-7000-8000-00000000cccc"}
	var procs []*Wakes
	for range 3 {
		procs = append(procs, e.wakes(t.TempDir()))
	}
	var wg sync.WaitGroup
	for _, w := range procs {
		for _, th := range threads {
			for i := range 4 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					w.Signal(context.Background(), th, fmt.Sprint("m", i), true, ok)
				}()
			}
		}
	}
	wg.Wait()
	per := map[string]int{}
	for _, r := range e.wakeRows() {
		per[r.thread]++
		if r.order != 0 {
			t.Errorf("thread %s row at order %d", r.thread, r.order)
		}
	}
	for _, th := range threads {
		if per[th] != 1 {
			t.Errorf("thread %s has %d wake rows, want 1", th, per[th])
		}
	}
}

func TestWakePayload_ExpectCodexQueueShape(t *testing.T) {
	var v struct {
		UserInput struct {
			Content []struct {
				Type, Text   string
				TextElements []any `json:"text_elements"`
			}
			ClientID string `json:"client_id"`
		}
	}
	p := wakePayload(wakeID(wThread, 3))
	if err := json.Unmarshal([]byte(p), &v); err != nil {
		t.Fatal(err)
	}
	if len(v.UserInput.Content) != 1 || v.UserInput.Content[0].Text != WakeText || v.UserInput.Content[0].Type != "text" || v.UserInput.ClientID != wakeID(wThread, 3) {
		t.Fatalf("payload %s", p)
	}
	if wakeID(wThread, 3) == wakeID(wThread, 4) || wakeID(wThread, 3) == wakeID("019a0000-0000-7000-8000-00000000bbbb", 3) {
		t.Fatal("wake ids collide")
	}
}

type stubThreads struct {
	adapter.Adapter
	loaded []string
}

func (s stubThreads) LoadedThreads(context.Context, ...string) []LoadedThread {
	var out []LoadedThread
	for _, id := range s.loaded {
		out = append(out, LoadedThread{ID: id})
	}
	return out
}

func newWakeHarness(t *testing.T, e *wakeEnv, loaded ...string) (*Harness, *fakeSys) {
	t.Helper()
	sys := newFakeSys()
	sys.own(wThread)
	c := newTestClaims(t, sys, &fakeRegistrar{})
	if err := c.AcceptDurable(context.Background(), claim(wThread)); err != nil {
		t.Fatal(err)
	}
	h := NewHarness(HarnessOptions{Adapter: stubThreads{Adapter: adapter.NewFake(adapter.Codex), loaded: loaded}})
	state := t.TempDir()
	if err := os.WriteFile(filepath.Join(state, SettingFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	h.setWakeRoute(&wakeRoute{stateDir: state, claims: c, wakes: e.wakes(t.TempDir())})
	return h, sys
}

func delivery(mode msg.Delivery, verified bool) adapter.Delivery {
	return adapter.Delivery{Mode: mode, VerifiedSender: verified, Target: adapter.Binding{Harness: adapter.Codex, SessionRef: wThread}}
}

func TestHarnessMailSignal_RouteTable_ExpectWakeOnlyForImmediateVerifiedOwnedUnhosted(t *testing.T) {
	cases := []struct {
		name     string
		d        adapter.Delivery
		loaded   []string
		lockGone bool
		rows     int
	}{
		{"immediate verified", delivery(msg.Immediate, true), nil, false, 1},
		{"next_turn", delivery(msg.NextTurn, true), nil, false, 0},
		{"unverified", delivery(msg.Immediate, false), nil, false, 0},
		{"hosted by an app-server", delivery(msg.Immediate, true), []string{wThread}, false, 0},
		{"owner proof fails at write time", delivery(msg.Immediate, true), nil, true, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newWakeEnv(t)
			h, sys := newWakeHarness(t, e, c.loaded...)
			if c.lockGone {
				sys.set(wThread, false, nil)
			}
			h.MailSignal(context.Background(), msg.Message{MessageID: "m1", Delivery: msg.Immediate}, c.d)
			if n := len(e.wakeRows()); n != c.rows {
				t.Fatalf("rows %d, want %d", n, c.rows)
			}
		})
	}
}

func TestHarnessMailSignal_NoRoute_ExpectNoOp(t *testing.T) {
	h := NewHarness(HarnessOptions{Adapter: stubThreads{Adapter: adapter.NewFake(adapter.Codex)}})
	if err := h.MailSignal(context.Background(), msg.Message{MessageID: "m1"}, delivery(msg.Immediate, true)); err != nil {
		t.Fatal(err)
	}
	if err := h.MailPulled(context.Background(), adapter.Binding{SessionRef: wThread}, false, nil); err != nil {
		t.Fatal(err)
	}
}

func TestHarnessMailPulled_ArrivalAfterSnapshot_ExpectNextGeneration(t *testing.T) {
	e := newWakeEnv(t)
	h, _ := newWakeHarness(t, e)
	h.MailSignal(context.Background(), msg.Message{MessageID: "m1"}, delivery(msg.Immediate, true))
	h.MailSignal(context.Background(), msg.Message{MessageID: "m2"}, delivery(msg.Immediate, true))
	e.take()
	if err := h.MailPulled(context.Background(), adapter.Binding{SessionRef: wThread}, false, func() ([]string, error) { return []string{"m2"}, nil }); err != nil {
		t.Fatal(err)
	}
	if n := len(e.wakeRows()); n != 1 {
		t.Fatalf("rows %d, want generation 2's", n)
	}
}

func TestQueueGateWakeInsert_JSONInvalidUserRow_ExpectWakeStillWritten(t *testing.T) {
	e := newWakeEnv(t)
	execFixture(t, e.queue, "INSERT INTO queued_items(id, thread_id, payload_json, queue_order, created_at_ms, updated_at_ms) VALUES ('bad', '"+wThread+"', 'not json', 0, 0, 0)")
	w := e.wakes(t.TempDir())
	e.signal(w, "m1")
	wantState(t, w, 1, GenQueued)
	if rs := e.wakeRows(); len(rs) != 1 || rs[0].order != 1 {
		t.Fatalf("rows %+v", rs)
	}

}

func TestWakes_OutcomeWriteFailsAfterCommit_ExpectSameProcessRecovers(t *testing.T) {
	for _, taken := range []bool{false, true} {
		t.Run(fmt.Sprint("taken=", taken), func(t *testing.T) {
			e := newWakeEnv(t)
			w := e.wakes(t.TempDir())
			fail := true
			w.crash = func(p string) error {
				if p == "after-commit" && fail {
					fail = false
					return errors.New("outcome write failed")
				}
				return nil
			}
			e.signal(w, "m1")
			wantState(t, w, 1, GenPending)
			if taken {
				e.take()
			}
			e.signal(w, "m2")
			wantGen := 1
			if taken {
				wantGen = 2
			}
			if g := gen(t, w); g.N != wantGen || g.State != GenQueued || len(e.wakeRows()) != 1 {
				t.Fatalf("generation %d %s, rows %d", g.N, g.State, len(e.wakeRows()))
			}
		})
	}
}

func TestWakes_PendingLeftInProcess_ExpectTickResolvesIt(t *testing.T) {
	e := newWakeEnv(t)
	w := e.wakes(t.TempDir())
	w.crash = func(p string) error {
		if p == "after-pending" {
			return errors.New("crash")
		}
		return nil
	}
	e.signal(w, "m1")
	w.crash = nil
	w.Tick(context.Background(), nil)
	wantState(t, w, 1, GenUnknown)
	if len(e.rows()) != 0 {
		t.Fatal("tick wrote a row")
	}
}

func TestWakes_DistinctMessagesFailedWakesAndPulls_ExpectEligibleDoesNotAccumulate(t *testing.T) {
	e := newWakeEnv(t)
	e.gate = func(g *QueueGate) {
		g.Version = func(context.Context, ownerproof.Proc) (string, error) { return "0.168.0", nil }
	}
	w := e.wakes(t.TempDir())
	for i := range 100 {
		id := fmt.Sprint("m", i)
		e.signal(w, id)
		w.Pulled(context.Background(), wThread, false, func() ([]string, error) { return nil, nil }, ok)
	}
	if g := wantState(t, w, 100, GenFailed); len(g.Eligible) != 0 {
		t.Fatalf("eligible kept %d drained ids", len(g.Eligible))
	}
	e.signal(w, "still-mailboxed")
	w.Pulled(context.Background(), wThread, false, func() ([]string, error) { return []string{"still-mailboxed"}, nil }, ok)
	if g := gen(t, w); len(g.Eligible) != 1 {
		t.Fatalf("eligible %v", g.Eligible)
	}
}

func TestHarnessWake_SettingTurnedOffWhileRunning_ExpectNoNewRowOnArrivalOrPull(t *testing.T) {
	e := newWakeEnv(t)
	h, _ := newWakeHarness(t, e)
	h.MailSignal(context.Background(), msg.Message{MessageID: "m1"}, delivery(msg.Immediate, true))
	h.MailSignal(context.Background(), msg.Message{MessageID: "m2"}, delivery(msg.Immediate, true))
	if n := len(e.wakeRows()); n != 1 {
		t.Fatalf("rows %d with the setting on", n)
	}
	e.take()
	os.Remove(filepath.Join(h.wakeRoute().stateDir, SettingFile))
	h.MailPulled(context.Background(), adapter.Binding{SessionRef: wThread}, false, func() ([]string, error) { return []string{"m2"}, nil })
	if n := len(e.wakeRows()); n != 0 {
		t.Fatalf("pull re-check wrote %d rows with the setting off", n)
	}
	if err := h.MailSignal(context.Background(), msg.Message{MessageID: "m3"}, delivery(msg.Immediate, true)); !errors.Is(err, ErrWakeOff) {
		t.Fatalf("err %v, want ErrWakeOff", err)
	}
	if n := len(e.wakeRows()); n != 0 {
		t.Fatalf("arrival wrote %d rows with the setting off", n)
	}
}

func TestWakes_TwoShimsShareTheRecord_ExpectOneGenerationAndConsistentRecord(t *testing.T) {
	for run := range 5 {
		e := newWakeEnv(t)
		e.gate = func(g *QueueGate) { g.BusyTimeout = 5 * time.Second }
		dir := t.TempDir()
		a, b := e.wakes(dir), e.wakes(dir)
		var wg sync.WaitGroup
		for i := range 8 {
			for _, w := range []*Wakes{a, b} {
				wg.Add(1)
				go func() {
					defer wg.Done()
					w.Signal(context.Background(), wThread, fmt.Sprint("m", i), true, ok)
				}()
			}
		}
		wg.Wait()
		if n := len(e.wakeRows()); n != 1 {
			t.Fatalf("run %d: rows %d", run, n)
		}
		wantState(t, a, 1, GenQueued)
	}
}

func TestWakes_TickSkipsThreadsNoLongerClaimed_ExpectRecordUntouched(t *testing.T) {
	e := newWakeEnv(t)
	w := e.wakes(t.TempDir())
	e.signal(w, "m1")
	e.take()
	w.Tick(context.Background(), func(string) bool { return false })
	wantState(t, w, 1, GenQueued)
	w.Tick(context.Background(), func(string) bool { return true })
	wantState(t, w, 1, GenAdmitted)
}

func TestWakes_CommitErrors_ExpectUnknownAndNextArrivalWritesOnlyIfFirstAbsent(t *testing.T) {
	cases := []struct {
		name     string
		atCommit string
		firstRow bool
	}{
		{"rolled back before COMMIT", "ROLLBACK", false},
		{"committed before COMMIT", "COMMIT", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newWakeEnv(t)
			e.gate = func(g *QueueGate) {
				g.atCommit = func(conn *sql.Conn) { conn.ExecContext(context.Background(), c.atCommit) }
			}
			w := e.wakes(t.TempDir())
			e.signal(w, "m1")
			wantState(t, w, 1, GenUnknown)
			if got := len(e.wakeRows()) == 1; got != c.firstRow {
				t.Fatalf("first row present %v, want %v", got, c.firstRow)
			}
			e.gate = nil
			e.signal(w, "m2")
			if n := len(e.wakeRows()); n != 1 {
				t.Fatalf("rows %d after the next arrival, want exactly 1", n)
			}
			if c.firstRow {
				wantState(t, w, 2, GenFailed)
			} else {
				wantState(t, w, 2, GenQueued)
			}
		})
	}
}

func TestWakes_RecordLockedByAnotherShim_ExpectTimeoutNoRowNoChange(t *testing.T) {
	e := newWakeEnv(t)
	w := e.wakes(t.TempDir())
	w.LockTimeout = 200 * time.Millisecond
	if err := ensurePrivateDir(w.Dir); err != nil {
		t.Fatal(err)
	}
	unlock, err := flockFile(filepath.Join(w.Dir, wThread+".lock"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	start := time.Now()
	if err := e.signal(w, "m1"); !errors.Is(err, ErrWakeLocked) {
		t.Fatalf("err %v, want ErrWakeLocked", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("waited %v", d)
	}
	if len(e.rows()) != 0 {
		t.Fatal("row written")
	}
	if _, err := os.Stat(w.path(wThread)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("record written without the lock: %v", err)
	}
}

func TestHarnessMailSignal_UnclaimedThreadLockHeldByParent_ExpectNoRow(t *testing.T) {
	e := newWakeEnv(t)
	h, sys := newWakeHarness(t, e)
	other := "019a0000-0000-7000-8000-00000000dddd"
	sys.own(other)
	d := delivery(msg.Immediate, true)
	d.Target.SessionRef = other
	if err := h.MailSignal(context.Background(), msg.Message{MessageID: "m1"}, d); !errors.Is(err, ErrNotClaimed) {
		t.Fatalf("err %v, want ErrNotClaimed", err)
	}
	if n := len(e.rows()); n != 0 {
		t.Fatalf("rows %d for an unclaimed thread", n)
	}
}

func TestWakes_Busy_ExpectExactlyOneWriteAttempt(t *testing.T) {
	e := newWakeEnv(t)
	db := openFixture(t, e.queue)
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.ExecContext(context.Background(), "BEGIN IMMEDIATE")
	defer conn.ExecContext(context.Background(), "ROLLBACK")
	w := e.wakes(t.TempDir())
	gate := w.Gate
	calls := 0
	w.Gate = func() *QueueGate { calls++; return gate() }
	e.signal(w, "m1")
	wantState(t, w, 1, GenFailed)
	if calls != 1 {
		t.Fatalf("gate transactions %d, want 1 (no retry beyond the busy timeout)", calls)
	}
}

// shortSockDir keeps a unix socket path under sun_path's 104 bytes.
func shortSockDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "ctw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func TestHarnessMailSignal_EndpointProbe_ExpectFailClosedOnProbeErrorOpenOnNoListener(t *testing.T) {
	cases := []struct {
		name   string
		listen bool
		rows   int
	}{
		{"no socket", false, 1},
		{"accepts then drops", true, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newWakeEnv(t)
			sock := filepath.Join(shortSockDir(t), "s")
			if c.listen {
				ln, err := net.Listen("unix", sock)
				if err != nil {
					t.Fatal(err)
				}
				defer ln.Close()
				go func() {
					for {
						cn, err := ln.Accept()
						if err != nil {
							return
						}
						cn.Close()
					}
				}()
			}
			h, _ := newWakeHarness(t, e)
			rt := h.wakeRoute()
			h = NewHarness(HarnessOptions{Adapter: &Adapter{ControlSocket: sock}})
			h.setWakeRoute(rt)
			err := h.MailSignal(context.Background(), msg.Message{MessageID: "m1"}, delivery(msg.Immediate, true))
			if n := len(e.wakeRows()); n != c.rows {
				t.Fatalf("rows %d, want %d (err %v)", n, c.rows, err)
			}
			if c.rows == 0 && !errors.Is(err, ErrHostProbe) {
				t.Fatalf("err %v, want ErrHostProbe", err)
			}
		})
	}
}
