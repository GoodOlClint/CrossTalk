package codex

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/harness"
	"github.com/GoodOlClint/crosstalk/internal/ownerproof"
)

var t0 = time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)

const (
	parentPID = 100
	shellPID  = 150
	hookPID   = 200
	outsider  = 300
	lockDir   = "/locks"
)

// fakeSys is the kernel as the proof reads it. Openers for a path is a
// sequence; each read takes the next list and the last one repeats.
type fakeSys struct {
	mu      sync.Mutex
	procs   map[int]ownerproof.Proc
	argv    map[int][]string
	held    map[string]bool
	open    map[string][][]int
	reads   map[string]int
	locks   map[string]int
	lockErr error
}

func (f *fakeSys) lockReads(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.locks[path]
}

func newFakeSys() *fakeSys {
	f := &fakeSys{procs: map[int]ownerproof.Proc{}, argv: map[int][]string{}, held: map[string]bool{}, open: map[string][][]int{}, reads: map[string]int{}, locks: map[string]int{}}
	f.proc(1, 0, t0.Add(-time.Hour), "launchd")
	f.proc(parentPID, 1, t0, "codex", "--no-daemon")
	f.proc(shellPID, parentPID, t0.Add(time.Minute), "sh", "-c", "crosstalk hook codex session-start")
	f.proc(hookPID, shellPID, t0.Add(time.Minute), "crosstalk", "hook", "codex", "session-start")
	f.proc(outsider, 1, t0.Add(time.Minute), "crosstalk", "hook", "codex", "session-start")
	return f
}

func (f *fakeSys) proc(pid, ppid int, start time.Time, argv ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.procs[pid] = ownerproof.Proc{PID: pid, PPID: ppid, Start: start}
	f.argv[pid] = argv
}

func (f *fakeSys) exit(pid int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.procs, pid)
}

// own makes the parent the holder and only opener of thread's lock.
func (f *fakeSys) own(thread string) { f.set(thread, true, []int{parentPID}) }

func (f *fakeSys) set(thread string, held bool, openers ...[]int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := lockPath(lockDir, thread)
	f.held[p] = held
	f.open[p] = openers
	f.reads[p] = 0
}

func (f *fakeSys) Info(pid int) (ownerproof.Proc, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.procs[pid]
	if !ok {
		return ownerproof.Proc{}, ownerproof.ErrNoProcess
	}
	return p, nil
}

func (f *fakeSys) Args(pid int) (string, []string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.procs[pid]; !ok {
		return "", nil, ownerproof.ErrNoProcess
	}
	a := f.argv[pid]
	return "/bin/" + a[0], a, nil
}

func (f *fakeSys) LockHeld(path string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.locks[path]++
	if f.lockErr != nil {
		return false, f.lockErr
	}
	h, ok := f.held[path]
	if !ok {
		return false, errors.New("no such file")
	}
	return h, nil
}

func (f *fakeSys) Openers(path string) ([]int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	seq := f.open[path]
	if len(seq) == 0 {
		return nil, nil
	}
	i := min(f.reads[path], len(seq)-1)
	f.reads[path]++
	return slices.Clone(seq[i]), nil
}

// fakeRegistrar records RegisterSession calls; fail makes a session's
// registration fail as a live agent held elsewhere would.
type fakeRegistrar struct {
	mu      sync.Mutex
	calls   []string
	retired []string
	fail    map[string]error
}

func (r *fakeRegistrar) RegisterSession(_ context.Context, req harness.RequestContext, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	tid, _ := req.Meta["threadId"].(string)
	r.calls = append(r.calls, tid)
	return r.fail[tid]
}

func (r *fakeRegistrar) RetireSession(_ context.Context, req harness.RequestContext) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	tid, _ := req.Meta["threadId"].(string)
	r.retired = append(r.retired, tid)
	return true, nil
}

func (r *fakeRegistrar) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls)
}

func newTestClaims(t *testing.T, sys *fakeSys, reg harness.SessionRegistrar) *Claims {
	t.Helper()
	parent, err := sys.Info(parentPID)
	if err != nil {
		t.Fatal(err)
	}
	return NewClaims(ClaimOptions{Sys: sys, Parent: parent, LockDir: lockDir, Registrar: reg, Tries: 3, Sleep: func(time.Duration) {}})
}

func claim(thread string) Claim { return Claim{SessionID: thread, Source: "startup", Cwd: "/work"} }

func TestLiveClaim_SenderOutsideParentAncestry_ExpectIgnored(t *testing.T) {
	sys, reg := newFakeSys(), &fakeRegistrar{}
	sys.own("T1")
	c := newTestClaims(t, sys, reg)
	if err := c.AcceptLive(context.Background(), outsider, claim("T1")); !errors.Is(err, ErrNotDescendant) {
		t.Fatalf("err %v, want ErrNotDescendant", err)
	}
	if c.Bound("T1") || len(reg.got()) != 0 {
		t.Fatalf("bound %v, registered %v", c.Bound("T1"), reg.got())
	}
}

func TestLiveClaim_BodyNamesParentButSenderOutsideAncestry_ExpectRejected(t *testing.T) {
	sys, reg := newFakeSys(), &fakeRegistrar{}
	sys.own("T1")
	c := newTestClaims(t, sys, reg)
	cl := claim("T1")
	cl.OwnerPID, cl.OwnerStartMicros, cl.ThreadEnv = parentPID, t0.UnixMicro(), "T1"
	if err := c.AcceptLive(context.Background(), outsider, cl); !errors.Is(err, ErrNotDescendant) {
		t.Fatalf("err %v, want ErrNotDescendant", err)
	}
	if c.Bound("T1") || len(reg.got()) != 0 {
		t.Fatal("a claim body stood in for transport ancestry")
	}
}

func TestLiveClaim_BodyPIDAndThreadEnvWrongButSenderInside_ExpectAcceptedByTransport(t *testing.T) {
	sys, reg := newFakeSys(), &fakeRegistrar{}
	sys.own("T1")
	c := newTestClaims(t, sys, reg)
	cl := claim("T1")
	cl.OwnerPID, cl.OwnerStartMicros, cl.ThreadEnv = outsider, 42, "T-other"
	if err := c.AcceptLive(context.Background(), hookPID, cl); err != nil {
		t.Fatal(err)
	}
	b, ok := c.Binding("T1")
	if !ok || !b.Live || !slices.Equal(reg.got(), []string{"T1"}) {
		t.Fatalf("binding %+v %v, registered %v", b, ok, reg.got())
	}
	if c.Bound("T-other") {
		t.Fatal("CODEX_THREAD_ID from the body was used to bind")
	}
}

func TestDurableClaim_ThreadParentDoesNotOwnLockFree_ExpectRejected(t *testing.T) {
	sys, reg := newFakeSys(), &fakeRegistrar{}
	sys.set("T1", false)
	c := newTestClaims(t, sys, reg)
	if err := c.AcceptDurable(context.Background(), claim("T1")); !errors.Is(err, ErrLockFree) {
		t.Fatalf("err %v, want ErrLockFree", err)
	}
	if c.Bound("T1") || len(reg.got()) != 0 {
		t.Fatal("bound a thread the parent does not own")
	}
}

func TestDurableClaim_MissingLockFile_ExpectRejected(t *testing.T) {
	sys, reg := newFakeSys(), &fakeRegistrar{}
	c := newTestClaims(t, sys, reg)
	if err := c.AcceptDurable(context.Background(), claim("T-none")); err == nil {
		t.Fatal("a missing lock file proved ownership")
	}
	if c.Bound("T-none") || len(reg.got()) != 0 {
		t.Fatal("bound")
	}
}

func TestClaim_LockFileOpenByParentButNotHeld_ExpectRejected(t *testing.T) {
	sys, reg := newFakeSys(), &fakeRegistrar{}
	sys.set("T1", false, []int{parentPID})
	c := newTestClaims(t, sys, reg)
	for _, accept := range []func() error{
		func() error { return c.AcceptLive(context.Background(), hookPID, claim("T1")) },
		func() error { return c.AcceptDurable(context.Background(), claim("T1")) },
	} {
		if err := accept(); !errors.Is(err, ErrLockFree) {
			t.Fatalf("err %v, want ErrLockFree", err)
		}
	}
	if c.Bound("T1") || len(reg.got()) != 0 {
		t.Fatal("bound")
	}
}

func TestClaim_PooledParentAppServer_ExpectRejected(t *testing.T) {
	sys, reg := newFakeSys(), &fakeRegistrar{}
	sys.proc(parentPID, 1, t0, "codex", "app-server", "--listen", "unix://")
	sys.own("T1")
	c := newTestClaims(t, sys, reg)
	if err := c.AcceptLive(context.Background(), hookPID, claim("T1")); !errors.Is(err, ErrPooled) {
		t.Fatalf("live: err %v, want ErrPooled", err)
	}
	if err := c.AcceptDurable(context.Background(), claim("T1")); !errors.Is(err, ErrPooled) {
		t.Fatalf("durable: err %v, want ErrPooled", err)
	}
	if c.Bound("T1") || len(reg.got()) != 0 {
		t.Fatal("bound")
	}
}

func TestClaim_PIDReusedAfterOwnerExit_ExpectRejected(t *testing.T) {
	sys, reg := newFakeSys(), &fakeRegistrar{}
	sys.own("T1")
	c := newTestClaims(t, sys, reg)
	sys.proc(parentPID, 1, t0.Add(time.Hour), "codex", "--no-daemon")
	if err := c.AcceptLive(context.Background(), hookPID, claim("T1")); err == nil {
		t.Fatal("live claim accepted under a reused parent pid")
	}
	if err := c.AcceptDurable(context.Background(), claim("T1")); !errors.Is(err, ErrParentChanged) {
		t.Fatalf("durable: err %v, want ErrParentChanged", err)
	}
	if c.Bound("T1") || len(reg.got()) != 0 {
		t.Fatal("bound")
	}
}

func TestRecheck_OwnerExitedAndPIDReused_ExpectRevoked(t *testing.T) {
	sys, reg := newFakeSys(), &fakeRegistrar{}
	sys.own("T1")
	c := newTestClaims(t, sys, reg)
	if err := c.AcceptDurable(context.Background(), claim("T1")); err != nil {
		t.Fatal(err)
	}
	sys.exit(parentPID)
	sys.proc(parentPID, 1, t0.Add(time.Hour), "codex", "--no-daemon")
	if got := c.Recheck(); !slices.Equal(got, []string{"T1"}) || c.Bound("T1") {
		t.Fatalf("revoked %v, still bound %v", got, c.Bound("T1"))
	}
}

func TestClaim_LockHeldByAnotherProcessParentNotOpener_ExpectRejected(t *testing.T) {
	sys, reg := newFakeSys(), &fakeRegistrar{}
	sys.set("T1", true, []int{outsider})
	c := newTestClaims(t, sys, reg)
	if err := c.AcceptLive(context.Background(), hookPID, claim("T1")); !errors.Is(err, ErrParentNotOpener) {
		t.Fatalf("err %v, want ErrParentNotOpener", err)
	}
	if c.Bound("T1") || len(reg.got()) != 0 {
		t.Fatal("bound")
	}
}

func TestClaim_SessionIDNotAThreadID_ExpectRejectedBeforeAnyRead(t *testing.T) {
	sys, reg := newFakeSys(), &fakeRegistrar{}
	sys.lockErr = errors.New("lock file read")
	c := newTestClaims(t, sys, reg)
	for _, sid := range []string{"", "../T1", "T1/../../x", "a b", string(make([]byte, 200))} {
		if err := c.AcceptDurable(context.Background(), claim(sid)); !errors.Is(err, ErrBadSessionID) {
			t.Fatalf("%q: err %v, want ErrBadSessionID", sid, err)
		}
	}
}

func TestProof_TransientSecondOpenerGoesAway_ExpectPasses(t *testing.T) {
	sys, reg := newFakeSys(), &fakeRegistrar{}
	sys.set("T1", true, []int{parentPID, outsider}, []int{parentPID})
	c := newTestClaims(t, sys, reg)
	if err := c.AcceptDurable(context.Background(), claim("T1")); err != nil {
		t.Fatalf("transient opener failed the proof: %v", err)
	}
	if !c.Bound("T1") {
		t.Fatal("not bound")
	}
}

func TestProof_SecondOpenerStays_ExpectFailsClosed(t *testing.T) {
	sys, reg := newFakeSys(), &fakeRegistrar{}
	sys.set("T1", true, []int{parentPID, outsider})
	c := newTestClaims(t, sys, reg)
	if err := c.AcceptDurable(context.Background(), claim("T1")); !errors.Is(err, ErrSecondOpener) {
		t.Fatalf("err %v, want ErrSecondOpener", err)
	}
	if c.Bound("T1") {
		t.Fatal("bound")
	}
	p := lockPath(lockDir, "T1")
	if sys.reads[p] != 3 {
		t.Fatalf("opener read %d times, want 3 (the debounce bound)", sys.reads[p])
	}
}

func TestRecheck_TransientSecondOpener_ExpectNotRevoked(t *testing.T) {
	sys, reg := newFakeSys(), &fakeRegistrar{}
	sys.own("T1")
	c := newTestClaims(t, sys, reg)
	if err := c.AcceptDurable(context.Background(), claim("T1")); err != nil {
		t.Fatal(err)
	}
	sys.set("T1", true, []int{parentPID, outsider}, []int{parentPID, outsider}, []int{parentPID})
	if got := c.Recheck(); len(got) != 0 || !c.Bound("T1") {
		t.Fatalf("revoked %v on a transient opener", got)
	}
}

func TestRecheck_OneThreadLosesItsLock_ExpectOnlyThatThreadRevoked(t *testing.T) {
	sys, reg := newFakeSys(), &fakeRegistrar{}
	sys.own("T1")
	sys.own("T2")
	c := newTestClaims(t, sys, reg)
	for _, th := range []string{"T1", "T2"} {
		if err := c.AcceptDurable(context.Background(), claim(th)); err != nil {
			t.Fatal(err)
		}
	}
	sys.set("T2", false)
	if got := c.Recheck(); !slices.Equal(got, []string{"T2"}) || !c.Bound("T1") || c.Bound("T2") {
		t.Fatalf("revoked %v; T1 bound %v, T2 bound %v", got, c.Bound("T1"), c.Bound("T2"))
	}
}

func TestClaims_RefireStartupResumeClearSameIDCompact_ExpectOneClaim(t *testing.T) {
	sys, reg := newFakeSys(), &fakeRegistrar{}
	sys.own("T1")
	c := newTestClaims(t, sys, reg)
	for i, src := range []string{"startup", "resume", "clear", "compact"} {
		cl := claim("T1")
		cl.Source = src
		var err error
		if i%2 == 0 {
			err = c.AcceptLive(context.Background(), hookPID, cl)
		} else {
			err = c.AcceptDurable(context.Background(), cl)
		}
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
	}
	if got := c.Threads(); !slices.Equal(got, []string{"T1"}) {
		t.Fatalf("claims %v, want one", got)
	}
	if b, _ := c.Binding("T1"); b.Claim.Source != "compact" {
		t.Fatalf("latest claim not kept: %+v", b)
	}
}

func TestClaims_ClearMakesNewThreadSameProcess_ExpectTwoClaims(t *testing.T) {
	sys, reg := newFakeSys(), &fakeRegistrar{}
	sys.own("T1")
	sys.own("T2")
	c := newTestClaims(t, sys, reg)
	if err := c.AcceptLive(context.Background(), hookPID, claim("T1")); err != nil {
		t.Fatal(err)
	}
	cl := claim("T2")
	cl.Source = "clear"
	if err := c.AcceptLive(context.Background(), hookPID, cl); err != nil {
		t.Fatal(err)
	}
	if got := c.Threads(); !slices.Equal(got, []string{"T1", "T2"}) {
		t.Fatalf("claims %v", got)
	}
}

func TestClaims_SessionHeldByAnotherLiveShim_ExpectIgnored(t *testing.T) {
	sys := newFakeSys()
	reg := &fakeRegistrar{fail: map[string]error{"T1": errors.New("rejected: agent_held")}}
	sys.own("T1")
	c := newTestClaims(t, sys, reg)
	if err := c.AcceptLive(context.Background(), hookPID, claim("T1")); err == nil {
		t.Fatal("claim for a session held elsewhere was accepted")
	}
	if c.Bound("T1") {
		t.Fatal("claim kept although registration failed")
	}
}

func TestClaims_ForeignClaimFails_ExpectExistingClaimNotRevoked(t *testing.T) {
	sys, reg := newFakeSys(), &fakeRegistrar{}
	sys.own("T1")
	c := newTestClaims(t, sys, reg)
	if err := c.AcceptLive(context.Background(), hookPID, claim("T1")); err != nil {
		t.Fatal(err)
	}
	if err := c.AcceptLive(context.Background(), outsider, claim("T1")); err == nil {
		t.Fatal("foreign claim accepted")
	}
	if !c.Bound("T1") || len(reg.got()) != 1 {
		t.Fatalf("foreign claim touched the binding: bound %v, registered %v", c.Bound("T1"), reg.got())
	}
}

// overlapSys records how many lock probes of one path run at once.
type overlapSys struct {
	*fakeSys
	mu       sync.Mutex
	inFlight int
	most     int
}

func (o *overlapSys) LockHeld(path string) (bool, error) {
	o.mu.Lock()
	o.inFlight++
	o.most = max(o.most, o.inFlight)
	o.mu.Unlock()
	time.Sleep(time.Millisecond)
	o.mu.Lock()
	o.inFlight--
	o.mu.Unlock()
	return o.fakeSys.LockHeld(path)
}

func TestProof_ConcurrentProbesOfOnePathAcrossTables_ExpectSerialized(t *testing.T) {
	sys := &overlapSys{fakeSys: newFakeSys()}
	sys.own("T1")
	parent, _ := sys.Info(parentPID)
	mk := func() *Claims {
		return NewClaims(ClaimOptions{Sys: sys, Parent: parent, LockDir: lockDir, Registrar: &fakeRegistrar{}, Tries: 1})
	}
	a, b := mk(), mk()
	var wg sync.WaitGroup
	for g := range 8 {
		c := a
		if g%2 == 1 {
			c = b
		}
		wg.Go(func() {
			for range 10 {
				c.proveTarget("T1")
			}
		})
	}
	wg.Wait()
	if sys.most != 1 {
		t.Fatalf("%d lock probes of one path overlapped in one process", sys.most)
	}
}

// heldSeq answers LockHeld from a sequence; the last answer repeats.
type heldSeq struct {
	*fakeSys
	mu  sync.Mutex
	seq []bool
}

func (h *heldSeq) LockHeld(path string) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	v := h.seq[0]
	if len(h.seq) > 1 {
		h.seq = h.seq[1:]
	}
	return v, nil
}

func TestProof_LockSeenHeldThenFreeAfterOpeners_ExpectLockFree(t *testing.T) {
	sys := &heldSeq{fakeSys: newFakeSys(), seq: []bool{true, false}}
	sys.own("T1")
	parent, _ := sys.Info(parentPID)
	c := NewClaims(ClaimOptions{Sys: sys, Parent: parent, LockDir: lockDir, Registrar: &fakeRegistrar{}, Tries: 1})
	if err := c.AcceptDurable(context.Background(), claim("T1")); !errors.Is(err, ErrLockFree) {
		t.Fatalf("err %v, want ErrLockFree from the read after the openers", err)
	}
}
