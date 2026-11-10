package codexqueue

import (
	"errors"
	"slices"
	"sync"
)

var (
	ErrPooled          = errors.New("shim parent is an app-server (pooled)")
	ErrNotDescendant   = errors.New("claim sender does not descend from the shim's parent")
	ErrLockFree        = errors.New("writer lock is not held")
	ErrParentNotOpener = errors.New("shim's parent does not have the lock file open")
	ErrSecondOpener    = errors.New("another process has the lock file open")
)

type Proc struct {
	PID   int
	Start int64
	Exe   string
	Args  []string
}

func (p Proc) sameAs(o Proc) bool { return p.PID == o.PID && p.Start == o.Start }

// System is what the proof reads from the kernel. Parent is the shim's own parent;
// Ancestry is the chain from a transport-authenticated sender pid upwards.
type System interface {
	Parent() Proc
	Ancestry(pid int) ([]Proc, error)
	LockHeld(thread string) (bool, error)
	Openers(thread string) ([]int, error)
}

// Claim is the hook's payload. The Body* fields are kept for diagnosis only and are never
// used to bind (plan 3.3: transport origin, not claim text).
type Claim struct {
	SessionID     string
	Source        string
	BodyPID       int
	BodyStart     int64
	BodyThreadEnv string
}

type Binding struct {
	Thread string
	Owner  Proc
	Live   bool
}

type CallResult int

const (
	CallUnbound CallResult = iota
	CallConfirmed
)

type Shim struct {
	mu     sync.Mutex
	sys    System
	claims map[string]*Binding
}

func NewShim(sys System) *Shim { return &Shim{sys: sys, claims: map[string]*Binding{}} }

// proveTarget is the lock-ownership inference (step 0 probe 13): the holder pid is not
// readable, so: the lock is held by someone, the parent has the file open, nobody else does.
func (s *Shim) proveTarget(thread string, parent Proc) error {
	for _, a := range parent.Args {
		if a == "app-server" {
			return ErrPooled
		}
	}
	held, err := s.sys.LockHeld(thread)
	if err != nil {
		return err
	}
	if !held {
		return ErrLockFree
	}
	openers, err := s.sys.Openers(thread)
	if err != nil {
		return err
	}
	if !slices.Contains(openers, parent.PID) {
		return ErrParentNotOpener
	}
	for _, o := range openers {
		if o != parent.PID {
			return ErrSecondOpener
		}
	}
	return nil
}

func (s *Shim) AcceptLive(senderPID int, c Claim) error {
	parent := s.sys.Parent()
	chain, err := s.sys.Ancestry(senderPID)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(chain, parent.sameAs) {
		return ErrNotDescendant
	}
	return s.accept(c, parent, true)
}

func (s *Shim) AcceptDurable(c Claim) error {
	return s.accept(c, s.sys.Parent(), false)
}

func (s *Shim) accept(c Claim, parent Proc, live bool) error {
	if err := s.proveTarget(c.SessionID, parent); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claims[c.SessionID] = &Binding{Thread: c.SessionID, Owner: parent, Live: live}
	return nil
}

func (s *Shim) Bound(thread string) bool { _, ok := s.Binding(thread); return ok }

func (s *Shim) Binding(thread string) (Binding, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b, ok := s.claims[thread]; ok {
		return *b, true
	}
	return Binding{}, false
}

func (s *Shim) Count() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.claims) }

// Call is a per-request binding by _meta.threadId: it confirms an existing claim and
// never creates one.
func (s *Shim) Call(thread string) CallResult {
	if s.Bound(thread) {
		return CallConfirmed
	}
	return CallUnbound
}

// Recheck revokes per thread: all claims if the parent changed, otherwise each claim whose
// own proof no longer holds.
func (s *Shim) Recheck() []string {
	parent := s.sys.Parent()
	s.mu.Lock()
	defer s.mu.Unlock()
	var revoked []string
	for t, b := range s.claims {
		if !b.Owner.sameAs(parent) || s.proveTarget(t, parent) != nil {
			delete(s.claims, t)
			revoked = append(revoked, t)
		}
	}
	slices.Sort(revoked)
	return revoked
}
