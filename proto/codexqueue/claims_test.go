package codexqueue

import (
	"errors"
	"sync"
	"testing"
)

// fakeSys is a scripted System. Process ids: codex 100 (start 1000) is the shim's parent.
type fakeSys struct {
	mu       sync.Mutex
	parent   Proc
	ancestry map[int][]Proc
	held     map[string]bool
	openers  map[string][]int
}

var codex = Proc{PID: 100, Start: 1000, Exe: "/usr/local/bin/codex", Args: []string{"codex", "--no-daemon"}}

func newFake() *fakeSys {
	return &fakeSys{
		parent:   codex,
		ancestry: map[int][]Proc{},
		held:     map[string]bool{},
		openers:  map[string][]int{},
	}
}

func (f *fakeSys) Parent() Proc { f.mu.Lock(); defer f.mu.Unlock(); return f.parent }
func (f *fakeSys) Ancestry(pid int) ([]Proc, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.ancestry[pid]
	if !ok {
		return nil, errors.New("no such process")
	}
	return a, nil
}
func (f *fakeSys) LockHeld(thread string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.held[thread], nil
}
func (f *fakeSys) Openers(thread string) ([]int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.openers[thread], nil
}

// own makes thread T owned by the parent: lock held, only the parent has the file open.
func (f *fakeSys) own(thread string) {
	f.mu.Lock()
	f.held[thread] = true
	f.openers[thread] = []int{codex.PID}
	f.mu.Unlock()
}

// hookUnder registers a hook process 500 whose ancestry reaches the codex parent.
func (f *fakeSys) hookUnder(parent Proc) {
	f.ancestry[500] = []Proc{{PID: 500, Start: 5000}, {PID: 499, Start: 4990}, parent}
}

func TestClaim_LiveHandoffOwnedThread_ExpectBound(t *testing.T) {
	f := newFake()
	f.own("T1")
	f.hookUnder(codex)
	s := NewShim(f)
	if err := s.AcceptLive(500, Claim{SessionID: "T1"}); err != nil || !s.Bound("T1") {
		t.Fatalf("err %v", err)
	}
}

func TestClaim_LiveSenderOutsideParentAncestry_ExpectRejectedNotDescendant(t *testing.T) {
	f := newFake()
	f.own("T1")
	f.ancestry[500] = []Proc{{PID: 500, Start: 5000}, {PID: 1, Start: 1}}
	s := NewShim(f)
	if err := s.AcceptLive(500, Claim{SessionID: "T1", BodyPID: codex.PID, BodyStart: codex.Start}); !errors.Is(err, ErrNotDescendant) || s.Bound("T1") {
		t.Fatalf("a body pid matching the parent must not substitute for transport ancestry: %v", err)
	}
}

func TestClaim_BodyFieldsDisagreeButTransportDescends_ExpectBoundBodyIgnored(t *testing.T) {
	f := newFake()
	f.own("T1")
	f.hookUnder(codex)
	s := NewShim(f)
	err := s.AcceptLive(500, Claim{SessionID: "T1", BodyPID: 999, BodyStart: 1, BodyThreadEnv: "T-other"})
	if err != nil || !s.Bound("T1") || s.Bound("T-other") {
		t.Fatalf("err %v", err)
	}
}

func TestClaim_PidReuseSameParentPidDifferentStart_ExpectRejectedNotDescendant(t *testing.T) {
	f := newFake()
	f.own("T1")
	f.hookUnder(Proc{PID: 100, Start: 2222})
	s := NewShim(f)
	if err := s.AcceptLive(500, Claim{SessionID: "T1"}); !errors.Is(err, ErrNotDescendant) {
		t.Fatalf("%v", err)
	}
}

func TestClaim_DurableForOwnedThread_ExpectBoundButNotLive(t *testing.T) {
	f := newFake()
	f.own("T1")
	s := NewShim(f)
	if err := s.AcceptDurable(Claim{SessionID: "T1"}); err != nil {
		t.Fatal(err)
	}
	if b, ok := s.Binding("T1"); !ok || b.Live {
		t.Fatalf("%+v %v", b, ok)
	}
}

func TestClaim_DurableForThreadParentDoesNotOwn_ExpectRejectedLockFree(t *testing.T) {
	f := newFake()
	s := NewShim(f)
	if err := s.AcceptDurable(Claim{SessionID: "T9"}); !errors.Is(err, ErrLockFree) || s.Bound("T9") {
		t.Fatalf("%v", err)
	}
}

func TestClaim_LockHeldByAnotherProcessParentHasNoFileOpen_ExpectRejectedParentNotOpener(t *testing.T) {
	f := newFake()
	f.held["T1"] = true
	f.openers["T1"] = []int{777}
	s := NewShim(f)
	if err := s.AcceptDurable(Claim{SessionID: "T1"}); !errors.Is(err, ErrParentNotOpener) {
		t.Fatalf("%v", err)
	}
}

func TestClaim_ParentHasFileOpenButLockFree_ExpectRejectedLockFree(t *testing.T) {
	f := newFake()
	f.openers["T1"] = []int{codex.PID}
	s := NewShim(f)
	if err := s.AcceptDurable(Claim{SessionID: "T1"}); !errors.Is(err, ErrLockFree) {
		t.Fatalf("%v", err)
	}
}

func TestClaim_SecondOpenerReader_ExpectRejectedFailsClosed(t *testing.T) {
	f := newFake()
	f.own("T1")
	f.openers["T1"] = []int{codex.PID, 888}
	s := NewShim(f)
	if err := s.AcceptDurable(Claim{SessionID: "T1"}); !errors.Is(err, ErrSecondOpener) {
		t.Fatalf("%v", err)
	}
}

func TestClaim_PooledShimParentIsAppServer_ExpectRejectedPooled(t *testing.T) {
	f := newFake()
	f.parent = Proc{PID: 100, Start: 1000, Args: []string{"codex", "app-server"}}
	f.own("T1")
	f.hookUnder(f.parent)
	s := NewShim(f)
	if err := s.AcceptLive(500, Claim{SessionID: "T1"}); !errors.Is(err, ErrPooled) || s.Bound("T1") {
		t.Fatalf("%v", err)
	}
}

func TestClaim_RefireSameSession_ExpectIdempotentOneBinding(t *testing.T) {
	f := newFake()
	f.own("T1")
	f.hookUnder(codex)
	s := NewShim(f)
	for _, src := range []string{"startup", "compact", "resume"} {
		if err := s.AcceptLive(500, Claim{SessionID: "T1", Source: src}); err != nil {
			t.Fatal(err)
		}
	}
	if s.Count() != 1 {
		t.Fatalf("%d bindings", s.Count())
	}
}

func TestClaim_ClearMakesNewThreadSameProcess_ExpectTwoBindingsOldStaysWhileLockHeld(t *testing.T) {
	f := newFake()
	f.own("T1")
	f.own("T2")
	f.hookUnder(codex)
	s := NewShim(f)
	s.AcceptLive(500, Claim{SessionID: "T1", Source: "startup"})
	s.AcceptLive(500, Claim{SessionID: "T2", Source: "clear"})
	if revoked := s.Recheck(); len(revoked) != 0 || s.Count() != 2 {
		t.Fatalf("revoked %v count %d", revoked, s.Count())
	}
}

func TestClaim_RecheckLockReleasedForOneThread_ExpectOnlyThatThreadRevoked(t *testing.T) {
	f := newFake()
	f.own("T1")
	f.own("T2")
	f.hookUnder(codex)
	s := NewShim(f)
	s.AcceptLive(500, Claim{SessionID: "T1"})
	s.AcceptLive(500, Claim{SessionID: "T2"})
	f.mu.Lock()
	f.held["T1"] = false
	f.mu.Unlock()
	revoked := s.Recheck()
	if len(revoked) != 1 || revoked[0] != "T1" || s.Bound("T1") || !s.Bound("T2") {
		t.Fatalf("revoked %v", revoked)
	}
}

func TestClaim_RecheckParentChangedStart_ExpectAllRevoked(t *testing.T) {
	f := newFake()
	f.own("T1")
	f.own("T2")
	f.hookUnder(codex)
	s := NewShim(f)
	s.AcceptLive(500, Claim{SessionID: "T1"})
	s.AcceptLive(500, Claim{SessionID: "T2"})
	f.mu.Lock()
	f.parent = Proc{PID: 100, Start: 7777, Args: codex.Args}
	f.mu.Unlock()
	if revoked := s.Recheck(); len(revoked) != 2 || s.Count() != 0 {
		t.Fatalf("revoked %v", revoked)
	}
}

func TestClaim_RecheckSecondOpenerAppearsLater_ExpectRevoked(t *testing.T) {
	f := newFake()
	f.own("T1")
	f.hookUnder(codex)
	s := NewShim(f)
	s.AcceptLive(500, Claim{SessionID: "T1"})
	f.mu.Lock()
	f.openers["T1"] = []int{codex.PID, 321}
	f.mu.Unlock()
	if revoked := s.Recheck(); len(revoked) != 1 || s.Bound("T1") {
		t.Fatalf("revoked %v", revoked)
	}
}

func TestClaim_ForgedClaimForBoundSessionFromOutsideAncestry_ExpectIgnoredNoRevoke(t *testing.T) {
	f := newFake()
	f.own("T1")
	f.hookUnder(codex)
	f.ancestry[600] = []Proc{{PID: 600, Start: 6000}, {PID: 1, Start: 1}}
	s := NewShim(f)
	s.AcceptLive(500, Claim{SessionID: "T1"})
	if err := s.AcceptLive(600, Claim{SessionID: "T1"}); !errors.Is(err, ErrNotDescendant) || !s.Bound("T1") {
		t.Fatalf("a foreign claim revoked or replaced a proven binding: %v", err)
	}
}

func TestClaim_PerRequestBindingForUnclaimedThread_ExpectUnboundNoClaimCreated(t *testing.T) {
	f := newFake()
	f.own("T1")
	f.own("T2")
	s := NewShim(f)
	f.hookUnder(codex)
	s.AcceptLive(500, Claim{SessionID: "T1"})
	if s.Call("T2") != CallUnbound || s.Bound("T2") {
		t.Fatal("a per-request thread id created a claim")
	}
	if s.Call("T1") != CallConfirmed {
		t.Fatal("per-request binding for a claimed thread must confirm it")
	}
}

func TestClaim_TwoThreadsOneProcessConcurrentCalls_ExpectNoCrossBindNoCrossRevoke(t *testing.T) {
	f := newFake()
	f.own("T1")
	f.own("T2")
	f.hookUnder(codex)
	s := NewShim(f)
	s.AcceptLive(500, Claim{SessionID: "T1"})
	s.AcceptLive(500, Claim{SessionID: "T2"})
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		for _, th := range []string{"T1", "T2", "T3"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				want := CallConfirmed
				if th == "T3" {
					want = CallUnbound
				}
				if got := s.Call(th); got != want {
					t.Errorf("%s: got %v want %v", th, got, want)
				}
			}()
		}
	}
	wg.Wait()
	f.mu.Lock()
	f.held["T1"] = false
	f.mu.Unlock()
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if got := s.Call("T2"); got != CallConfirmed {
				t.Errorf("T2 lost its binding while T1 was revoked: %v", got)
			}
		}()
		go func() { defer wg.Done(); s.Recheck() }()
	}
	wg.Wait()
	if s.Bound("T1") || !s.Bound("T2") {
		t.Fatalf("T1 bound %v, T2 bound %v: revoking one thread must leave the other", s.Bound("T1"), s.Bound("T2"))
	}
}
